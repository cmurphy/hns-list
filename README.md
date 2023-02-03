Hierarchical Namespaces Resource List Extension
===============================================

This project is a Kubernetes [API
Extension](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/apiserver-aggregation/)
and [kubectl
plugin](https://kubernetes.io/docs/tasks/extend-kubectl/kubectl-plugins/) to
add on top of [Hierarchical
Namespaces](https://github.com/kubernetes-sigs/hierarchical-namespaces) to view
resources in a hierarchical tree of namespaces.

Even though HNC namespaces are organized in a tree structure, Kubernetes still
sees the resources contained in them as flat. To view resources under one node,
we would have to find every child namespace under the parent and request
resources for each one individually, or list resources for all namespaces and
filter them by our set of namespaces.

This extension allows us to call a single endpoint to get all resources of a
given kind for all namespaces under a given parent.

Build
-----

Build the kubectl-hns-list plugin and install it in your GOPATH:

```
make cli
```

Build the docker image for the server:

```
make server
```

Install
-------

1. Install [cert-manager](https://cert-manager.io/docs/installation/)

2. Install [hierarchical
   namespaces](https://github.com/kubernetes-sigs/hierarchical-namespaces/releases/)

2. Apply the manifest:

```
kubectl apply -f manifest/manifest.yaml
```

Usage
-----

See the [hierarchical namespaces user
guide](https://github.com/kubernetes-sigs/hierarchical-namespaces/tree/master/docs/user-guide)
for detailed information on creating and using hierarchical namespaces.

The hierarchy can be viewed with the kubectl-hns plugin:

```
$ kubectl hns tree parent1
parent1
├── [s] child1
│   └── [s] grandchild1
└── [s] child2
    └── [s] grandchild2

[s] indicates subnamespaces
```

After building and installing the kubectl-hns-list plugin, view resources for
all namespaces under a given parent, including the parent:

```
kubectl hns list --namespace parent1 get secret
NAMESPACE     NAME   AGE
child1        s1     22s
child1        s2     20s
grandchild2   s1     7s
parent1       s1     27s
```

The list can be watched with `--watch/-w`.

You can list or watch resources in all namespaces with `--all-namespaces/-A`.
This is equivalent to running the same resource request without the plugin with
`--all-namespaces/-A`.
