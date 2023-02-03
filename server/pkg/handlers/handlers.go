package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/sync/semaphore"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/cmurphy/hns-list/pkg/apiresources"
	"github.com/cmurphy/hns-list/pkg/consts"
	"github.com/gorilla/mux"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

const (
	FieldSelectorKey = "fieldSelector"
	workers          = int64(3)
	hnsLabelSuffix   = ".tree.hnc.x-k8s.io/depth"
)

var (
	paramScheme = runtime.NewScheme()
	paramCodec  = runtime.NewParameterCodec(paramScheme)
)

func init() {
	metav1.AddToGroupVersion(paramScheme, metav1.SchemeGroupVersion)
}

func DiscoveryHandler(apis apiresources.APIResourceWatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		logrus.Tracef("handling request %s\n", req.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"kind":         "APIResourceList",
			"apiVersion":   "v1",
			"groupVersion": consts.GroupVersion,
			"resources":    apis.List(),
		}
		apiResourceBytes, err := json.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(apiResourceBytes)
	}
}

func Forwarder(dynamicClient dynamic.Interface, apis apiresources.APIResourceWatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logrus.Tracef("handling request %s\n", r.URL.Path)
		vars := mux.Vars(r)
		resource, err := gvrFromVars(vars, apis)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		resourceClient := dynamicClient.Resource(resource)
		opts := metav1.ListOptions{}
		paramCodec.DecodeParameters(r.URL.Query(), metav1.SchemeGroupVersion, &opts)

		if opts.Watch {
			watcher, err := getWatchers(r.Context(), resourceClient, nil, opts)
			if isErrorAndHandleError(w, err) {
				return
			}
			watchHandler(w, r, resourceClient, watcher, opts)
			return
		}
		resources, err := resourceClient.List(r.Context(), opts)
		if isErrorAndHandleError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		returnResp(w, resources.UnstructuredContent())
	}
}

func NamespaceHandler(apis apiresources.APIResourceWatcher, namespaceCache corecontrollers.NamespaceCache, dynamicClient dynamic.Interface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logrus.Tracef("handling request %s\n", r.URL.Path)

		vars := mux.Vars(r)
		namespace := vars["namespace"]
		resource, err := gvrFromVars(vars, apis)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		resourceClient := dynamicClient.Resource(resource)

		opts := metav1.ListOptions{}
		paramCodec.DecodeParameters(r.URL.Query(), metav1.SchemeGroupVersion, &opts)

		label := namespace + hnsLabelSuffix
		selector, err := labels.Parse(label)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		namespaces, err := namespaceCache.List(selector)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if opts.Watch {
			watchers, err := getWatchers(r.Context(), resourceClient, namespaces, opts)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			watchHandler(w, r, resourceClient, watchers, opts)
			return
		}
		listHandler(w, r, resource, resourceClient, namespaces, opts, apis)
		return
	}
}

func getWatchers(ctx context.Context, client dynamic.NamespaceableResourceInterface, namespaces []*corev1.Namespace, opts metav1.ListOptions) ([]watch.Interface, error) {
	if len(namespaces) == 0 {
		watcher, err := client.Watch(ctx, opts)
		if err != nil {
			return nil, err
		}
		return []watch.Interface{watcher}, nil
	}
	watcherChan := make(chan watch.Interface)
	done := make(chan bool)
	watchers := make([]watch.Interface, 0)
	go func() {
		for watcher := range watcherChan {
			watchers = append(watchers, watcher)
		}
		done <- true
	}()
	eg := new(errgroup.Group)
	for _, ns := range namespaces {
		ns := ns.Name
		eg.Go(func() error {
			watcher, err := client.Namespace(ns).Watch(ctx, opts)
			if err != nil {
				return err
			}
			watcherChan <- watcher
			return nil
		})
	}
	err := eg.Wait()
	if err != nil {
		return nil, err
	}
	close(watcherChan)
	<-done
	return watchers, nil
}

func watchHandler(w http.ResponseWriter, r *http.Request, client dynamic.NamespaceableResourceInterface, watchers []watch.Interface, opts metav1.ListOptions) {
	events := make(chan metav1.WatchEvent)
	doneEvents := make(chan bool)

	go func() {
		w.Header().Set("Content-Type", "application/json")
		for event := range events {
			returnResp(w, event)
			w.(http.Flusher).Flush()
		}
		doneEvents <- true
	}()

	eg, ctx := errgroup.WithContext(r.Context())
	for _, watcher := range watchers {
		watcher := watcher
		eg.Go(func() error {
			for {
				select {
				case event := <-watcher.ResultChan():
					outEvent, err := convertEvent(event)
					if err != nil {
						return err
					}
					events <- *outEvent
				case <-ctx.Done():
					return nil
				}
			}
		})
	}
	err := eg.Wait()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	if ctx.Err() == context.Canceled {
		logrus.Debugf("client disconnected: %v", r.RemoteAddr)
	}
	<-doneEvents
	return
}

func listHandler(w http.ResponseWriter, r *http.Request, resource schema.GroupVersionResource, client dynamic.NamespaceableResourceInterface, namespaces []*corev1.Namespace, opts metav1.ListOptions, apis apiresources.APIResourceWatcher) {
	results := make(chan unstructured.Unstructured)
	resourceCollection := make([]unstructured.Unstructured, 0)
	latestResourceVersion := 0
	resourceVersions := make(chan int)
	doneResources := make(chan bool)
	doneVersions := make(chan bool)

	go func() {
		for r := range results {
			resourceCollection = append(resourceCollection, r)
		}
		doneResources <- true
	}()

	go func() {
		for r := range resourceVersions {
			if r > latestResourceVersion {
				latestResourceVersion = r
			}
		}
		doneVersions <- true
	}()

	eg, ctx := errgroup.WithContext(r.Context())
	sem := semaphore.NewWeighted(workers)
	for _, ns := range namespaces {
		ns := ns.Name
		if err := sem.Acquire(ctx, 1); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		eg.Go(func() error {
			defer sem.Release(1)
			resourcesForNamespace, err := client.Namespace(ns).List(ctx, opts)
			if err != nil {
				return err
			}
			if resourcesForNamespace != nil && len(resourcesForNamespace.Items) > 0 {
				rv, err := strconv.Atoi(resourcesForNamespace.GetResourceVersion())
				if err != nil {
					rv = 0
				}
				resourceVersions <- rv
				for _, r := range resourcesForNamespace.Items {
					results <- r
				}
			}
			return nil
		})
	}
	err := eg.Wait()
	if isErrorAndHandleError(w, err) {
		return
	}
	close(results)
	close(resourceVersions)
	<-doneResources
	<-doneVersions

	err = sortCollection(resourceCollection)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := responseData(resource, apis.GetKindForResource(resource)+"List", strconv.Itoa(latestResourceVersion), resourceCollection)
	w.Header().Set("Content-Type", "application/json")
	returnResp(w, resp)
}

func sortCollection(resourceCollection []unstructured.Unstructured) error {
	var err error
	sort.Slice(resourceCollection, func(i, j int) bool {
		metaI, ok := resourceCollection[i].Object["metadata"].(map[string]interface{})
		if !ok {
			err = fmt.Errorf("could not sort invalid resource")
			return false
		}
		metaJ, ok := resourceCollection[j].Object["metadata"].(map[string]interface{})
		if !ok {
			err = fmt.Errorf("could not sort invalid resource")
			return false
		}
		if metaI["namespace"].(string) < metaJ["namespace"].(string) {
			return true
		}
		if metaI["namespace"].(string) > metaJ["namespace"].(string) {
			return false
		}
		return metaI["name"].(string) < metaJ["name"].(string)
	})
	return err
}

func gvrFromVars(vars map[string]string, apis apiresources.APIResourceWatcher) (schema.GroupVersionResource, error) {
	groupResource := strings.Split(vars["resource"], ".")
	group := ""
	if len(groupResource) > 1 {
		group = strings.Join(groupResource[:len(groupResource)-1], ".")
	}
	resourceName := groupResource[len(groupResource)-1]
	resource, ok := apis.Get(resourceName, group)
	if !ok {
		return schema.GroupVersionResource{}, fmt.Errorf("could not find resource %s", vars["resource"])
	}
	return schema.GroupVersionResource{Group: group, Version: resource.Version, Resource: resourceName}, nil
}

func convertEvent(event watch.Event) (*metav1.WatchEvent, error) {
	internalEvent := metav1.InternalEvent(event)
	outEvent := &metav1.WatchEvent{}
	err := metav1.Convert_v1_InternalEvent_To_v1_WatchEvent(&internalEvent, outEvent, nil)
	if err != nil {
		return nil, err
	}
	return outEvent, nil
}

func returnResp(w http.ResponseWriter, resp interface{}) {
	resourceJSON, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(resourceJSON)
	w.Write([]byte("\n"))
	return
}

func responseData(resource schema.GroupVersionResource, kind, resourceVersion string, items []unstructured.Unstructured) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": resource.GroupVersion().String(),
		"kind":       kind,
		"metadata": map[string]interface{}{
			"resourceVersion": resourceVersion,
		},
		"items": items,
	}
}

func isErrorAndHandleError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return true
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
	return true
}
