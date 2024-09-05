package main

import (
	"context"
	"fmt"
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
	"log"
	"os"
	"path/filepath"
	"strings"
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
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					queue.Add(key)
					log.Printf("Secret added: %s", key)
				}
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				if err == nil {
					if strings.HasSuffix(key, "-argocd-cluster") {
						// Пропускаем обработку удаления производных секретов ArgoCD
						log.Printf("Skibidi deletion for derived ArgoCD secret: %s", key)
					} else {
						queue.Add(key)
						log.Printf("Secret deletion detected: %s", key)
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
					log.Printf("Invalid key type: %v", obj)
					return
				}

				namespace, name, err := cache.SplitMetaNamespaceKey(key)
				if err != nil {
					log.Printf("Error splitting key: %v", err)
					return
				}

				secret, err := clientset.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
				if errors.IsNotFound(err) {
					// Handle deletion
					handleDeleteSecret(clientset, namespace, fmt.Sprintf("%s-argocd-cluster", name))
				} else if err != nil {
					log.Printf("Error getting secret: %v", err)
					return
				} else {
					// Handle add or update
					handleAddOrUpdateSecret(clientset, secret)
				}
			}(obj)
		}
	}()

	stop := make(chan struct{})
	defer close(stop)
	controller.Run(stop)
}

func handleAddOrUpdateSecret(clientset *kubernetes.Clientset, secret *corev1.Secret) {
	if val, ok := secret.Labels["cluster"]; !ok || val != "true" {
		log.Printf("Secret %s does not have label 'cluster: true', skipping", secret.Name)
		return
	}

	kubeconfigData, ok := secret.Data["kubeconfig"]
	if !ok {
		log.Printf("kubeconfig not found in secret %s, skipping", secret.Name)
		return
	}

	var kubeconfig interface{}
	if err := yaml.Unmarshal(kubeconfigData, &kubeconfig); err != nil {
		log.Printf("Error parsing kubeconfig: %v", err)
		return
	}

	kubeconfigMap := convertToStringKeyMap(kubeconfig).(map[string]interface{})

	server := kubeconfigMap["clusters"].([]interface{})[0].(map[string]interface{})["cluster"].(map[string]interface{})["server"].(string)
	token := kubeconfigMap["users"].([]interface{})[0].(map[string]interface{})["user"].(map[string]interface{})["token"].(string)

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
		log.Printf("Error creating derived secret: %v", err)
	} else {
		log.Printf("Derived secret %s created successfully", newSecret.Name)
	}
}

func handleDeleteSecret(clientset *kubernetes.Clientset, namespace, name string) {
	err := clientset.CoreV1().Secrets(namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
	if err != nil {
		log.Printf("Error deleting derived secret: %v", err)
	} else {
		log.Printf("Derived secret %s deleted successfully", name)
	}
}
