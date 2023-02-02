package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
)

var watch bool

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
		d := newDoer(args[0])
		rv, err := d.get(args[0])
		if err != nil {
			return err
		}
		if watch {
			return d.watch(args[0], rv)
		}
		return nil
	},
}

func init() {
	getCmd.Flags().BoolVarP(&watch, "watch", "w", false, "watch for changes")
}

type doer struct {
	client  dynamic.ResourceInterface
	printer printers.ResourcePrinter
}

func newDoer(resource string) doer {
	client := getClient(resource)
	return doer{
		client:  client,
		printer: printers.NewTypeSetter(scheme.Scheme).ToPrinter(printers.NewTablePrinter(printers.PrintOptions{})),
	}
}

func getClient(resource string) dynamic.ResourceInterface {
	gvr := GroupVersion.WithResource(resource)
	resourceClient := client.Resource(gvr)
	if namespace != "" {
		return resourceClient.Namespace(namespace)
	}
	return resourceClient
}

func (d doer) get(resource string) (string, error) {
	unst, err := d.client.List(context.TODO(), metav1.ListOptions{})
	rv := unst.GetResourceVersion()
	if len(unst.Items) == 0 {
		fmt.Printf("No resources found in %s namespace", namespace)
		return rv, nil
	}
	unst.SetAPIVersion("resources.hns.demo/v1alpha1")
	kind := unst.Items[0].GetKind()
	unst.SetKind(kind + "List")
	if err = d.printer.PrintObj(unst, os.Stdout); err != nil {
		return rv, err
	}
	return rv, nil
}

func (d doer) watch(resource, rv string) error {
	if rv == "" {
		rv = "0"
	}
	opts := metav1.ListOptions{
		ResourceVersion: rv,
	}
	watcher, err := d.client.Watch(context.TODO(), opts)
	if err != nil {
		return err
	}
	for event := range watcher.ResultChan() {
		if err = d.printer.PrintObj(event.Object, os.Stdout); err != nil {
			return err
		}
	}
	return nil
}
