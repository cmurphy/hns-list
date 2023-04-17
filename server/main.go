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

	"github.com/cmurphy/hns-list/pkg/apiresources"
	"github.com/cmurphy/hns-list/pkg/handlers"
	"github.com/gorilla/mux"
	"github.com/rancher/wrangler/pkg/generated/controllers/apiextensions.k8s.io"
	"github.com/rancher/wrangler/pkg/generated/controllers/apiregistration.k8s.io"
	"github.com/rancher/wrangler/pkg/generated/controllers/core"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
		w := watcher(ctx, cfg)
		server(ctx, c, w, cfg)
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

func watcher(ctx context.Context, cfg *rest.Config) apiresources.APIResourceWatcher {
	discovery, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		logrus.Fatalf("could not start watcher: %v", err)
	}
	crdFactory, err := apiextensions.NewFactoryFromConfig(cfg)
	if err != nil {
		logrus.Fatalf("could not start watcher: %v", err)
	}
	crd := crdFactory.Apiextensions().V1().CustomResourceDefinition()
	apiFactory, err := apiregistration.NewFactoryFromConfig(cfg)
	if err != nil {
		logrus.Fatalf("could not start watcher: %v", err)
	}
	apiService := apiFactory.Apiregistration().V1().APIService()
	apiResourceWatcher := apiresources.WatchAPIResources(ctx, discovery, crd, apiService)
	crdFactory.Start(ctx, 50)
	apiFactory.Start(ctx, 50)
	return apiResourceWatcher
}

func server(ctx context.Context, c *cli.Context, apis apiresources.APIResourceWatcher, cfg *rest.Config) {
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		logrus.Fatal(err)
		return
	}
	coreFactory, err := core.NewFactoryFromConfig(cfg)
	if err != nil {
		logrus.Fatal(err)
	}
	namespaceCache := coreFactory.Core().V1().Namespace().Cache()
	configMapCache := coreFactory.Core().V1().ConfigMap().Cache()
	coreFactory.Start(ctx, 50)
	mux := mux.NewRouter()
	mux.HandleFunc("/apis/resources.hns.demo/v1alpha1", handlers.DiscoveryHandler(apis))
	mux.HandleFunc("/apis/resources.hns.demo/v1alpha1/{resource}", handlers.Forwarder(dynamicClient, apis))
	mux.HandleFunc("/apis/resources.hns.demo/v1alpha1/namespaces/{namespace}/{resource}", handlers.NamespaceHandler(apis, namespaceCache, dynamicClient))
	mux.Use(handlers.AuthenticateMiddleware(configMapCache))

	address := c.String("host") + ":" + c.String("port")
	clientCA, err := getClientCA(coreFactory.Core().V1().ConfigMap())
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

func getClientCA(configMapClient corecontrollers.ConfigMapClient) (string, error) {
	config, err := configMapClient.Get(handlers.KubeSystemNamespace, handlers.ExtensionConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	clientCA, ok := config.Data[handlers.ClientCAKey]
	if !ok {
		return "", fmt.Errorf("invalid extension config")
	}
	return string(clientCA), nil
}
