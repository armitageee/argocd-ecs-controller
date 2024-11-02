package main
import (
  "time"
  "bytes"
	"context"
	"encoding/base64"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)


var (
	cpuUsageGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "app_cpu_usage",
		Help: "CPU usage of the application",
	})
	memoryUsageGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "app_memory_usage",
		Help: "Memory usage of the application",
	})
	secretsAddedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "secrets_added_total",
		Help: "Total number of secrets added",
	})
	secretsUpdatedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "secrets_updated_total",
		Help: "Total number of secrets updated",
	})
	secretsDeletedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "secrets_deleted_total",
		Help: "Total number of secrets deleted",
	})
)

func init() {
	// Register Prometheus metrics
	prometheus.MustRegister(cpuUsageGauge, memoryUsageGauge, secretsAddedCounter, secretsUpdatedCounter, secretsDeletedCounter)
}

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

func isBase64Encoded(data []byte) bool {
	_, err := base64.StdEncoding.DecodeString(string(data))
	return err == nil
}

func main() {
	// Настройка логгера logrus
	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		TimestampFormat: "2006-01-02 15:04:05",
		FullTimestamp:   true,
	})

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		logger.Infof("Starting Prometheus metrics endpoint on :2112/metrics")
		if err := http.ListenAndServe(":2112", nil); err != nil {
			logger.Fatalf("Error starting metrics endpoint: %v", err)
		}
	}()

	config, err := rest.InClusterConfig()
	if err != nil {
		if home := homedir.HomeDir(); home != "" {
			config, err = clientcmd.BuildConfigFromFlags("", filepath.Join(home, ".kube", "config"))
			if err != nil {
				logger.Fatalf("Failed to build config: %v", err)
			}
		} else {
			logger.Fatalf("Failed to build in-cluster config: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Fatalf("Failed to create clientset: %v", err)
	}

	// Watch for secrets
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
					secretsAddedCounter.Inc()
					logger.Infof("Secret added: %s", key)
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldSecret, ok1 := oldObj.(*corev1.Secret)
				newSecret, ok2 := newObj.(*corev1.Secret)
				if ok1 && ok2 && secretNeedsUpdate(oldSecret, newSecret) {
					key, err := cache.MetaNamespaceKeyFunc(newObj)
					if err == nil {
						queue.Add(key)
						secretsUpdatedCounter.Inc()
						logger.Infof("Secret updated: %s", key)
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				if err == nil {
					queue.Add(key)
					secretsDeletedCounter.Inc()
					logger.Infof("Secret deletion detected: %s", key)
				}
			},
		},
	)

	// Monitor CPU and memory usage periodically
	go monitorSystemUsage(logger)

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
					logger.Errorf("Invalid key type: %v", obj)
					return
				}

				namespace, name, err := cache.SplitMetaNamespaceKey(key)
				if err != nil {
					logger.Errorf("Error splitting key: %v", err)
					return
				}

				secret, err := clientset.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
				if errors.IsNotFound(err) {
					handleDeleteSecret(clientset, namespace, fmt.Sprintf("%s-argocd-cluster", name), logger)
				} else if err != nil {
					logger.Errorf("Error getting secret: %v", err)
					return
				} else {
					handleAddOrUpdateSecret(clientset, secret, logger)
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

func handleAddOrUpdateSecret(clientset *kubernetes.Clientset, secret *corev1.Secret, logger *logrus.Logger) {
	if val, ok := secret.Labels["cluster"]; !ok || val != "true" {
		logger.Infof("Secret %s does not have label 'cluster: true', skipping", secret.Name)
		return
	}

	kubeconfigData, ok := secret.Data["kubeconfig"]
	if !ok {
		logger.Infof("kubeconfig not found in secret %s, skipping", secret.Name)
		return
	}

	// Проверка, закодированы ли данные в base64
	var decodedKubeconfigData []byte
	if isBase64Encoded(kubeconfigData) {
		logger.Infof("Kubeconfig is in base64 format, decoding...")
		decodedData, err := base64.StdEncoding.DecodeString(string(kubeconfigData))
		if err != nil {
			logger.Errorf("Error decoding base64 kubeconfig: %v", err)
			return
		}
		decodedKubeconfigData = decodedData
	} else {
		decodedKubeconfigData = kubeconfigData
	}

	var kubeconfig interface{}
	if err := yaml.Unmarshal(decodedKubeconfigData, &kubeconfig); err != nil {
		logger.Errorf("Error parsing kubeconfig: %v", err)
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
			logger.Errorf("Error updating derived secret: %v", err)
		} else {
			logger.Infof("Derived secret %s updated successfully", existingSecret.Name)
		}
	} else if errors.IsNotFound(err) {
		// Секрет не существует, создаём его
		createNewSecret(clientset, secret, server, token, logger)
	} else {
		logger.Errorf("Error retrieving derived secret: %v", err)
	}
}

func createNewSecret(clientset *kubernetes.Clientset, secret *corev1.Secret, server, token string, logger *logrus.Logger) {
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
		logger.Errorf("Error creating new derived secret: %v", err)
	} else {
		logger.Infof("Derived secret %s created successfully", newSecret.Name)
	}
}

func handleDeleteSecret(clientset *kubernetes.Clientset, namespace, name string, logger *logrus.Logger) {
	_, err := clientset.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		logger.Infof("Derived secret %s already deleted or does not exist, skipping", name)
		return
	} else if err != nil {
		logger.Errorf("Error checking for secret before deletion: %v", err)
		return
	}
	err = clientset.CoreV1().Secrets(namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
	if err != nil {
		logger.Errorf("Error deleting derived secret: %v", err)
	} else {
		logger.Infof("Derived secret %s deleted successfully", name)
	}
}

func monitorSystemUsage() {
	var m runtime.MemStats
	for {
		runtime.ReadMemStats(&m)
		cpuUsageGauge.Set(float64(runtime.NumGoroutine()))
		memoryUsageGauge.Set(float64(m.Alloc) / 1024 / 1024) // MB
		time.Sleep(10 * time.Second)
	}
}
