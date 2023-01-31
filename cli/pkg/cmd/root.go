package cmd

import (
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	apischeme "sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	rootCmd       *cobra.Command
	namespace     string
	k8sClient     *kubernetes.Clientset
	GroupVersion  = schema.GroupVersion{Group: "resources.hns.demo", Version: "v1alpha1"}
	schemeBuilder = &apischeme.Builder{GroupVersion: GroupVersion}
	client        *rest.RESTClient
)

func init() {
	utilruntime.Must(schemeBuilder.AddToScheme(scheme.Scheme))

	kubecfgFlags := genericclioptions.NewConfigFlags(false)

	rootCmd = &cobra.Command{
		Use:   "kubectl-hns-list",
		Short: "list resources in hierarchical namespaces",
		PersistentPreRunE: func(c *cobra.Command, args []string) error {
			config, err := kubecfgFlags.ToRESTConfig()
			if err != nil {
				return err
			}

			// create the K8s clientset
			k8sClient, err = kubernetes.NewForConfig(config)
			if err != nil {
				return err
			}

			// create the hns-list clientset
			listConfig := *config
			listConfig.ContentConfig.GroupVersion = &GroupVersion
			listConfig.APIPath = "/apis"
			listConfig.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: scheme.Codecs}
			listConfig.UserAgent = rest.DefaultKubernetesUserAgent()
			client, err = rest.UnversionedRESTClientFor(&listConfig)
			if err != nil {
				return err
			}

			return nil
		},
	}
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "parent namespace to scope request to")

	rootCmd.AddCommand(getCmd)
}

func Execute() error {
	return rootCmd.Execute()
}
