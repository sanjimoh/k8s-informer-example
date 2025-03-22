package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	corev1 "k8s.io/api/core/v1"
)

func main() {
	// Create client configuration
	var kubeconfig *rest.Config
	var err error

	// Check if running inside cluster or outside
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath != "" {
		// Out-of-cluster configuration
		kubeconfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			panic(fmt.Errorf("unable to load kubeconfig: %v", err))
		}
	} else {
		// In-cluster configuration
		kubeconfig, err = rest.InClusterConfig()
		if err != nil {
			panic(fmt.Errorf("unable to create in-cluster config: %v", err))
		}
	}

	// Create the clientset
	clientset, err := kubernetes.NewForConfig(kubeconfig)
	if err != nil {
		panic(fmt.Errorf("unable to create clientset: %v", err))
	}

	// Create a SharedInformerFactory
	// The second parameter is the resync period
	factory := informers.NewSharedInformerFactory(clientset, 30*time.Minute)

	// Get the Pod informer
	podInformer := factory.Core().V1().Pods().Informer()

	// Create a channel to handle stopping the informers
	stopper := make(chan struct{})
	defer close(stopper)

	// Start all informers
	factory.Start(stopper)

	// Wait for the initial sync to complete
	if !cache.WaitForCacheSync(stopper, podInformer.HasSynced) {
		log.Fatal("Failed to sync caches")
	}

	log.Println("Informer caches synced successfully!")

	// Add event handlers before starting the informer
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			fmt.Printf("Pod added: %s/%s\n", pod.Namespace, pod.Name)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*corev1.Pod)
			newPod := newObj.(*corev1.Pod)
			fmt.Printf("Pod updated: %s/%s\n", newPod.Namespace, newPod.Name)
			fmt.Printf("Old pod: %s/%s\n", oldPod.Namespace, oldPod.Name)
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				// When a delete is observed, sometimes the object is a DeletedFinalStateUnknown
				// type instead of the actual object
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					fmt.Printf("Error decoding object, invalid type\n")
					return
				}
				pod, ok = tombstone.Obj.(*corev1.Pod)
				if !ok {
					fmt.Printf("Error decoding object tombstone, invalid type\n")
					return
				}
			}
			fmt.Printf("Pod deleted: %s/%s\n", pod.Namespace, pod.Name)
		},
	})

	// Create a work queue
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	defer queue.ShutDown()

	// Add event handlers that enqueue items
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(newObj)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	})

	// Start a number of worker go routines
	for i := 0; i < 5; i++ {
		go wait.Until(func() {
			processQueue(queue, podInformer.GetIndexer())
		}, time.Second, stopper)
	}

	// Keep the application running
	<-stopper
}

func processQueue(queue workqueue.RateLimitingInterface, indexer cache.Indexer) {
	for {
		// Get the next item
		key, quit := queue.Get()
		if quit {
			return
		}

		// Tell the queue we're done with this key when the function exits
		defer queue.Done(key)

		// Process the item
		err := processItemWithTimeout(key.(string), indexer)
		if err == nil {
			// If no error, tell the queue we've finished processing this key
			queue.Forget(key)
		} else if queue.NumRequeues(key) < 5 {
			// If there was an error but we're not at max retries, requeue
			handleFailure(key.(string), queue, err)
		} else {
			// Too many retries
			fmt.Printf("Error processing %s (giving up): %v\n", key, err)
			queue.Forget(key)
			runtime.HandleError(err)
		}
	}
}

func processItemWithTimeout(key string, indexer cache.Indexer) error {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create a channel for result and error
	resultChan := make(chan error, 1)

	// Process in a goroutine
	go func() {
		resultChan <- processItem(key, indexer)
	}()

	// Wait for either result or timeout
	select {
	case err := <-resultChan:
		return err
	case <-ctx.Done():
		return fmt.Errorf("processing timed out for %s", key)
	}
}

func processItem(key string, indexer cache.Indexer) error {
	// Split the key into namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	// Get the object from the indexer
	obj, exists, err := indexer.GetByKey(key)
	if err != nil {
		return fmt.Errorf("error fetching object with key %s from store: %v", key, err)
	}

	if !exists {
		// Object was deleted
		fmt.Printf("Pod %s/%s was deleted\n", namespace, name)
		return nil
	}

	// Object was added or updated
	pod := obj.(*corev1.Pod)
	fmt.Printf("Processing pod %s/%s with phase %s\n", namespace, name, pod.Status.Phase)

	// Implement your business logic here

	return nil
}

func handleFailure(key string, queue workqueue.RateLimitingInterface, err error) {
	// Log the error
	runtime.HandleError(fmt.Errorf("error processing %s: %v", key, err))

	// Re-queue with backoff
	queue.AddRateLimited(key)
}
