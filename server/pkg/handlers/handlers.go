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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
			callback := func(event *metav1.WatchEvent) {
				returnResp(w, event)
				w.(http.Flusher).Flush()
			}
			err = watch(r.Context(), w, r, resourceClient, opts, callback)
		} else {
			err = list(w, r, resourceClient, opts)
		}

		if apierrors.IsNotFound(err) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
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

		results := make(chan unstructured.Unstructured)
		eg, ctx := errgroup.WithContext(r.Context())
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

		events := make(chan metav1.WatchEvent)
		doneEvents := make(chan bool)

		go func() {
			for event := range events {
				returnResp(w, event)
				w.(http.Flusher).Flush()
			}
			doneEvents <- true
		}()

		sem := semaphore.NewWeighted(workers)
		for _, ns := range namespaces {
			ns := ns.Name
			if err := sem.Acquire(ctx, 1); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			eg.Go(func() error {
				defer sem.Release(1)
				if opts.Watch {
					callback := func(event *metav1.WatchEvent) { events <- *event }
					err = watch(ctx, w, r, resourceClient.Namespace(ns), opts, callback)
				} else {
					resourcesForNamespace, err := resourceClient.Namespace(ns).List(ctx, opts)
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
				}
				return nil
			})
		}
		err = eg.Wait()
		if apierrors.IsNotFound(err) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		close(results)
		close(resourceVersions)
		close(events)
		<-doneResources
		<-doneVersions
		<-doneEvents

		if opts.Watch {
			return
		}

		err = sortCollection(resourceCollection)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		resp := responseData(resource, apis.GetKindForResource(resource)+"List", strconv.Itoa(latestResourceVersion), resourceCollection)
		returnResp(w, resp)
	}
}

func watch(ctx context.Context, w http.ResponseWriter, r *http.Request, client dynamic.ResourceInterface, opts metav1.ListOptions, callback func(event *metav1.WatchEvent)) error {
	clientGone := w.(http.CloseNotifier).CloseNotify()
	watcher, err := client.Watch(ctx, opts)
	if err != nil {
		return err
	}

	for {
		select {
		case event := <-watcher.ResultChan():
			internalEvent := metav1.InternalEvent(event)
			outEvent := &metav1.WatchEvent{}
			err := metav1.Convert_v1_InternalEvent_To_v1_WatchEvent(&internalEvent, outEvent, nil)
			if err != nil {
				return err
			}
			callback(outEvent)
		case <-clientGone:
			logrus.Debugf("client disconnected: %v", r.RemoteAddr)
			return nil
		}
	}
}

func list(w http.ResponseWriter, r *http.Request, client dynamic.ResourceInterface, opts metav1.ListOptions) error {
	resources, err := client.List(r.Context(), opts)
	if err != nil {
		return err
	}

	returnResp(w, resources.UnstructuredContent())
	return nil
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

func returnResp(w http.ResponseWriter, resp interface{}) {
	resourceJSON, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
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
