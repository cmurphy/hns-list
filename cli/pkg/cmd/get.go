package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/kubernetes/scheme"
)

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "list resources",
	RunE: func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("not enough arguments to 'get'")
		}
		if len(args) > 1 {
			return fmt.Errorf("too many arguments to 'get'")
		}
		resource := args[0]
		return get(resource)
	},
}

func get(resource string) error {
	unst := unstructured.UnstructuredList{}
	err := client.Get().Resource(resource).Namespace(namespace).Do(context.TODO()).Into(&unst)
	if err != nil {
		return err
	}
	if len(unst.Items) == 0 {
		fmt.Println("No resources found in %s namespace", namespace)
	}
	unst.SetAPIVersion("resources.hns.demo/v1alpha1")
	kind := unst.Items[0].GetKind()
	unst.SetKind(kind + "List")
	ptr := printers.NewTypeSetter(scheme.Scheme).ToPrinter(printers.NewTablePrinter(printers.PrintOptions{}))
	if err = ptr.PrintObj(&unst, os.Stdout); err != nil {
		return err
	}
	return nil
}
