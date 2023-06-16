// Package main starts a watch on API resources and starts the server.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cmurphy/hns-list/pkg/apiresources"
	"github.com/cmurphy/hns-list/pkg/handlers"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corecache "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	apiregv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
)

func main() {
	ctx := context.Background()
	app := cli.NewApp()
	app.Name = "hns-list"
	app.Usage = "hns extension"
	app.Action = func(c *cli.Context) error {
		if c.Bool("debug") {
			logrus.SetLevel(logrus.DebugLevel)
		}
		if c.Bool("trace") {
			logrus.SetLevel(logrus.TraceLevel)
		}
		cfg, err := getConfig(c.String("kubeconfig"))
		if err != nil {
			logrus.Fatalf("could not start watcher: %v", err)
		}
		server(ctx, c, cfg)
		return nil
	}
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "host",
			Usage:  "address to listen on",
			Value:  "0.0.0.0",
			EnvVar: "LISTEN_ADDRESS",
		},
		cli.StringFlag{
			Name:   "port",
			Usage:  "TLS port to listen on",
			Value:  "7443",
			EnvVar: "LISTEN_PORT",
		},
		cli.StringFlag{
			Name:   "certpath",
			Usage:  "path to cert",
			EnvVar: "CERTPATH",
		},
		cli.StringFlag{
			Name:   "keypath",
			Usage:  "path to key",
			EnvVar: "KEYPATH",
		},
		cli.StringFlag{
			Name:   "kubeconfig",
			Usage:  "path to kubeconfig",
			EnvVar: "KUBECONFIG",
		},
		cli.BoolFlag{
			Name:   "debug",
			Usage:  "debug logs",
			EnvVar: "DEBUG",
		},
		cli.BoolFlag{
			Name:   "trace",
			Usage:  "trace logs",
			EnvVar: "TRACE",
		},
	}
	app.Run(os.Args)
}

func getConfig(kubeconfig string) (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("could not get kubeconfig: %w", err)
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func server(ctx context.Context, c *cli.Context, cfg *rest.Config) {
	discovery, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		logrus.Fatalf("could not start watcher: %v", err)
	}
	clientGetter := handlers.ClientGetter(cfg)
	dynamicFactory, err := getDynamicInformerFactory(cfg)
	if err != nil {
		logrus.Fatal(err)
	}
	crdInformer, apiServiceInformer := setUpAPIInformers(dynamicFactory, ctx.Done())
	apis := apiresources.WatchAPIResources(ctx, discovery, crdInformer, apiServiceInformer)
	factory, err := getInformerFactory(cfg)
	if err != nil {
		logrus.Fatal(err)
	}
	namespaceCache, configMapCache, err := setUpInformers(factory, ctx.Done())
	if err != nil {
		logrus.Fatal(err)
	}
	mux := mux.NewRouter()
	mux.HandleFunc("/apis/resources.hns.demo/v1alpha1", handlers.DiscoveryHandler(apis))
	mux.HandleFunc("/apis/resources.hns.demo/v1alpha1/{resource}", handlers.Forwarder(clientGetter, apis))
	mux.HandleFunc("/apis/resources.hns.demo/v1alpha1/namespaces/{namespace}/{resource}", handlers.NamespaceHandler(clientGetter, apis, namespaceCache))
	mux.Use(handlers.AuthenticateMiddleware(configMapCache))

	address := c.String("host") + ":" + c.String("port")
	clientCA, err := getClientCA(configMapCache)
	if err != nil {
		logrus.Fatal(err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM([]byte(clientCA))
	tlsConfig := &tls.Config{
		ClientCAs:  caCertPool,
		ClientAuth: tls.RequireAndVerifyClientCert,
	}
	server := http.Server{
		Addr:      address,
		Handler:   mux,
		TLSConfig: tlsConfig,
	}
	logrus.Infof("starting server on %s", address)
	err = server.ListenAndServeTLS(c.String("certpath"), c.String("keypath"))
	if err != nil {
		logrus.Fatal(err)
	}
}

func getClientCA(configMapCache corecache.ConfigMapNamespaceLister) (string, error) {
	config, err := configMapCache.Get(handlers.ExtensionConfigMap)
	if err != nil {
		return "", err
	}
	clientCA, ok := config.Data[handlers.ClientCAKey]
	if !ok {
		return "", fmt.Errorf("invalid extension config")
	}
	return string(clientCA), nil
}

func getInformerFactory(cfg *rest.Config) (informers.SharedInformerFactory, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return informers.NewSharedInformerFactory(clientset, 0), nil
}

func getDynamicInformerFactory(cfg *rest.Config) (dynamicinformer.DynamicSharedInformerFactory, error) {
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, time.Minute), nil
}

func setUpInformers(factory informers.SharedInformerFactory, stop <-chan struct{}) (corecache.NamespaceLister, corecache.ConfigMapNamespaceLister, error) {
	namespaceInformer := factory.Core().V1().Namespaces()
	configMapInformer := factory.Core().V1().ConfigMaps()
	go factory.Start(stop)
	if !cache.WaitForCacheSync(stop, namespaceInformer.Informer().HasSynced, configMapInformer.Informer().HasSynced) {
		return nil, nil, fmt.Errorf("cached failed to sync")
	}
	return namespaceInformer.Lister(), configMapInformer.Lister().ConfigMaps(handlers.KubeSystemNamespace), nil
}

func setUpAPIInformers(factory dynamicinformer.DynamicSharedInformerFactory, stop <-chan struct{}) (cache.SharedIndexInformer, cache.SharedIndexInformer) {
	crdGVR := apiextv1.SchemeGroupVersion.WithResource("customresourcedefinitions")
	crdInformer := factory.ForResource(crdGVR).Informer()
	apiServiceGVR := apiregv1.SchemeGroupVersion.WithResource("apiservices")
	apiServiceInformer := factory.ForResource(apiServiceGVR).Informer()
	go factory.Start(stop)
	return crdInformer, apiServiceInformer
}
