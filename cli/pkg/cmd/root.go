package cmd

import (
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	apischeme "sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	rootCmd       *cobra.Command
	namespace     string
	allNamespaces bool
	GroupVersion  = schema.GroupVersion{Group: "resources.hns.demo", Version: "v1alpha1"}
	schemeBuilder = &apischeme.Builder{GroupVersion: GroupVersion}
	client        *dynamic.DynamicClient
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

			client, err = dynamic.NewForConfig(config)
			if err != nil {
				return err
			}
			return nil
		},
		SilenceUsage: true,
	}
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "parent namespace to scope request to")
	rootCmd.PersistentFlags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "list across all namespaces")

	rootCmd.AddCommand(getCmd)
}

func Execute() error {
	return rootCmd.Execute()
}
