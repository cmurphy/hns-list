package handlers

import (
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
		resources, err := resourceClient.List(r.Context(), opts)
		if apierrors.IsNotFound(err) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
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

		sem := semaphore.NewWeighted(workers)
		for _, ns := range namespaces {
			ns := ns.Name
			if err := sem.Acquire(ctx, 1); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			eg.Go(func() error {
				defer sem.Release(1)
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
				return nil
			})
		}
		err = eg.Wait()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		close(results)
		close(resourceVersions)
		<-doneResources
		<-doneVersions

		var sortErr error
		sort.Slice(resourceCollection, func(i, j int) bool {
			metaI, ok := resourceCollection[i].Object["metadata"].(map[string]interface{})
			if !ok {
				sortErr = fmt.Errorf("could not sort invalid resource")
				return false
			}
			metaJ, ok := resourceCollection[j].Object["metadata"].(map[string]interface{})
			if !ok {
				sortErr = fmt.Errorf("could not sort invalid resource")
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
		if sortErr != nil {
			http.Error(w, sortErr.Error(), http.StatusInternalServerError)
			return
		}

		resp := responseData(resource, apis.GetKindForResource(resource)+"List", strconv.Itoa(latestResourceVersion), resourceCollection)
		returnResp(w, resp)
	}
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

func returnResp(w http.ResponseWriter, resp map[string]interface{}) {
	resourceJSON, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(resourceJSON)
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
