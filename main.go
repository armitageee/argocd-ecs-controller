package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
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
	log := logrus.New()
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})
	log.SetOutput(os.Stdout)
	log.SetLevel(logrus.InfoLevel)

	config, err := rest.InClusterConfig()
	if err != nil {
		if home := homedir.HomeDir(); home != "" {
			config, err = clientcmd.BuildConfigFromFlags("", filepath.Join(home, ".kube", "config"))
			if err != nil {
				log.Fatalf("Failed to build config: %v", err)
			}
		} else {
			log.Fatalf("Failed to build in-cluster config: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
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
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				secret, ok := obj.(*corev1.Secret)
				if ok && secret.Labels["cluster"] == "true" {
					key, err := cache.MetaNamespaceKeyFunc(obj)
					if err == nil {
						queue.Add(key)
						log.Infof("Secret added: %s", key)
					}
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldSecret, ok1 := oldObj.(*corev1.Secret)
				newSecret, ok2 := newObj.(*corev1.Secret)
				if ok1 && ok2 && secretNeedsUpdate(oldSecret, newSecret) {
					key, err := cache.MetaNamespaceKeyFunc(newObj)
					if err == nil {
						queue.Add(key)
						log.Infof("Secret updated: %s", key)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				secret, ok := obj.(*corev1.Secret)
				if ok && secret.Labels["cluster"] == "true" {
					key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
					if err == nil {
						queue.Add(key)
						log.Infof("Secret deletion detected: %s", key)
					}
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
					log.Errorf("Invalid key type: %v", obj)
					return
				}

				namespace, name, err := cache.SplitMetaNamespaceKey(key)
				if err != nil {
					log.Errorf("Error splitting key: %v", err)
					return
				}

				secret, err := clientset.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
				if errors.IsNotFound(err) {
					log.Infof("Derived secret %s already deleted", name)
				} else if err != nil {
					log.Errorf("Error getting secret: %v", err)
					return
				} else {
					handleAddOrUpdateSecret(clientset, secret, log)
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

func handleAddOrUpdateSecret(clientset kubernetes.Interface, secret *corev1.Secret, log *logrus.Logger) {
	if val, ok := secret.Labels["cluster"]; !ok || val != "true" {
		log.Infof("Secret %s does not have label 'cluster: true', skipping", secret.Name)
		return
	}

	kubeconfigData, ok := secret.Data["kubeconfig"]
	if !ok {
		log.Infof("kubeconfig not found in secret %s, skipping", secret.Name)
		return
	}

	var kubeconfig interface{}
	if err := yaml.Unmarshal(kubeconfigData, &kubeconfig); err != nil {
		log.Errorf("Error parsing kubeconfig: %v", err)
		return
	}

	kubeconfigMap := convertToStringKeyMap(kubeconfig).(map[string]interface{})
	server := kubeconfigMap["clusters"].([]interface{})[0].(map[string]interface{})["cluster"].(map[string]interface{})["server"].(string)
	token := kubeconfigMap["users"].([]interface{})[0].(map[string]interface{})["user"].(map[string]interface{})["token"].(string)

	derivedSecretName := fmt.Sprintf("%s-argocd-cluster", secret.Name)
	_, err := clientset.CoreV1().Secrets(secret.Namespace).Get(context.Background(), derivedSecretName, metav1.GetOptions{})

	if errors.IsNotFound(err) {
		// Секрет не найден, создаём его
		createNewSecret(clientset, secret, server, token, log)
	} else if err != nil {
		log.Errorf("Error retrieving derived secret: %v", err)
	} else {
		log.Infof("Derived secret %s already exists, no action required", derivedSecretName)
	}
}

func createNewSecret(clientset kubernetes.Interface, secret *corev1.Secret, server, token string, log *logrus.Logger) {
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
		log.Errorf("Error creating new derived secret: %v", err)
	} else {
		log.Infof("Derived secret %s created successfully", newSecret.Name)
	}
}
