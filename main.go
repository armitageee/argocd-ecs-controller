package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/client-go/util/workqueue"
)

func convertToStringKeyMap(input interface{}) interface{} {
	switch in := input.(type) {
	case map[interface{}]interface{}:
		out := make(map[string]interface{})
		for key, value := range in {
			strKey, ok := key.(string)
			if !ok {
				continue // Skip non-string keys
			}
			out[strKey] = convertToStringKeyMap(value)
		}
		return out
	case []interface{}:
		for i, v := range in {
			in[i] = convertToStringKeyMap(v)
		}
	}
	return input
}

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	sugar := logger.Sugar()

	config, err := rest.InClusterConfig()
	if err != nil {
		if home := homedir.HomeDir(); home != "" {
			config, err = clientcmd.BuildConfigFromFlags("", filepath.Join(home, ".kube", "config"))
			if err != nil {
				sugar.Fatalf("Failed to build config: %v", err)
			}
		} else {
			sugar.Fatalf("Failed to build in-cluster config: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		sugar.Fatalf("Failed to create clientset: %v", err)
	}

	watchlist := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"secrets",
		os.Getenv("NAMESPACE"),
		fields.Everything(),
	)

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	_, controller := cache.NewInformer(
		watchlist,
		&corev1.Secret{},
		0, // No resync period
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					queue.Add(key)
					sugar.Infof("Secret added: %s", key)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldSecret, ok1 := oldObj.(*corev1.Secret)
				newSecret, ok2 := newObj.(*corev1.Secret)
				if ok1 && ok2 && secretNeedsUpdate(oldSecret, newSecret) {
					key, err := cache.MetaNamespaceKeyFunc(newObj)
					if err == nil {
						queue.Add(key)
						sugar.Infof("Secret updated: %s", key)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				if err == nil {
					queue.Add(key)
					sugar.Infof("Secret deletion detected: %s", key)
				}
			},
		},
	)

	go func() {
		for {
			obj, shutdown := queue.Get()
			if shutdown {
				break
			}

			func(obj interface{}) {
				defer queue.Done(obj)
				key, ok := obj.(string)
				if !ok {
					sugar.Errorf("Invalid key type: %v", obj)
					return
				}

				namespace, name, err := cache.SplitMetaNamespaceKey(key)
				if err != nil {
					sugar.Errorf("Error splitting key: %v", err)
					return
				}

				secret, err := clientset.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
				if errors.IsNotFound(err) {
					handleDeleteSecret(clientset, namespace, fmt.Sprintf("%s-argocd-cluster", name), sugar)
				} else if err != nil {
					sugar.Errorf("Error getting secret: %v", err)
					return
				} else {
					handleAddOrUpdateSecret(clientset, secret, sugar)
				}
			}(obj)
		}
	}()

	stop := make(chan struct{})
	defer close(stop)
	controller.Run(stop)
}

func secretNeedsUpdate(oldSecret, newSecret *corev1.Secret) bool {
	oldData, oldOk := oldSecret.Data["kubeconfig"]
	newData, newOk := newSecret.Data["kubeconfig"]

	if oldOk && newOk && !bytes.Equal(oldData, newData) {
		return true
	}
	return false
}

func handleAddOrUpdateSecret(clientset *kubernetes.Clientset, secret *corev1.Secret, sugar *zap.SugaredLogger) {
	if val, ok := secret.Labels["cluster"]; !ok || val != "true" {
		sugar.Infof("Secret %s does not have label 'cluster: true', skipping", secret.Name)
		return
	}

	kubeconfigData, ok := secret.Data["kubeconfig"]
	if !ok {
		sugar.Infof("kubeconfig not found in secret %s, skipping", secret.Name)
		return
	}

	var kubeconfig interface{}
	if err := yaml.Unmarshal(kubeconfigData, &kubeconfig); err != nil {
		sugar.Errorf("Error parsing kubeconfig: %v", err)
		return
	}

	kubeconfigMap := convertToStringKeyMap(kubeconfig).(map[string]interface{})
	server := kubeconfigMap["clusters"].([]interface{})[0].(map[string]interface{})["cluster"].(map[string]interface{})["server"].(string)
	token := kubeconfigMap["users"].([]interface{})[0].(map[string]interface{})["user"].(map[string]interface{})["token"].(string)

	existingSecret, err := clientset.CoreV1().Secrets(secret.Namespace).Get(context.Background(), fmt.Sprintf("%s-argocd-cluster", secret.Name), metav1.GetOptions{})
	if err == nil {
		// Секрет существует, обновляем его
		existingSecret.StringData = map[string]string{
			"name":       secret.Name,
			"server":     server,
			"namespaces": "",
			"config": fmt.Sprintf(`{
                "bearerToken": "%s",
                "tlsClientConfig": {
                    "insecure": false
                }
            }`, token),
		}
		_, err = clientset.CoreV1().Secrets(secret.Namespace).Update(context.Background(), existingSecret, metav1.UpdateOptions{})
		if err != nil {
			sugar.Errorf("Error updating derived secret: %v", err)
		} else {
			sugar.Infof("Derived secret %s updated successfully", existingSecret.Name)
		}
	} else if errors.IsNotFound(err) {
		// Секрет не существует, создаём его
		createNewSecret(clientset, secret, server, token, sugar)
	} else {
		sugar.Errorf("Error retrieving derived secret: %v", err)
	}
}

func createNewSecret(clientset *kubernetes.Clientset, secret *corev1.Secret, server, token string, sugar *zap.SugaredLogger) {
	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-argocd-cluster", secret.Name),
			Labels: map[string]string{
				"argocd.argoproj.io/secret-type": "cluster",
			},
		},
		Type: "Opaque",
		StringData: map[string]string{
			"name":       secret.Name,
			"server":     server,
			"namespaces": "",
			"config": fmt.Sprintf(`{
                "bearerToken": "%s",
                "tlsClientConfig": {
                    "insecure": false
                }
            }`, token),
		},
	}

	_, err := clientset.CoreV1().Secrets(secret.Namespace).Create(context.Background(), newSecret, metav1.CreateOptions{})
	if err != nil {
		sugar.Errorf("Error creating new derived secret: %v", err)
	} else {
		sugar.Infof("Derived secret %s created successfully", newSecret.Name)
	}
}

func handleDeleteSecret(clientset *kubernetes.Clientset, namespace, name string, sugar *zap.SugaredLogger) {
	err := clientset.CoreV1().Secrets(namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
	if err != nil {
		sugar.Errorf("Error deleting derived secret: %v", err)
	} else {
		sugar.Infof("Derived secret %s deleted successfully", name)
	}
}
