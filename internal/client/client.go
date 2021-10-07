package client

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	unstructuredv1 "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

const (
	clientQPS   = 200
	clientBurst = 400
)

type GetOptions struct {
	APIResource APIResource
	Namespace   string
}

type ListOptions struct {
	APIResources []APIResource
	Namespaces   []string
}

type Interface interface {
	ResolveAPIResource(s string) (*APIResource, error)
	Get(ctx context.Context, name string, opts GetOptions) (*unstructuredv1.Unstructured, error)
	List(ctx context.Context, opts ListOptions) (*unstructuredv1.UnstructuredList, error)
}

type client struct {
	configFlags *Flags

	discoveryClient discovery.DiscoveryInterface
	dynamicClient   dynamic.Interface
	mapper          meta.RESTMapper
}

func (f *Flags) ToClient() (Interface, error) {
	config, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	config.WarningHandler = rest.NoWarnings{}
	config.QPS = clientQPS
	config.Burst = clientBurst
	f.WithDiscoveryBurst(clientBurst)

	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dis, err := f.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	mapper, err := f.ToRESTMapper()
	if err != nil {
		return nil, err
	}
	cli := &client{
		configFlags:     f,
		discoveryClient: dis,
		dynamicClient:   dyn,
		mapper:          mapper,
	}

	return cli, nil
}

func (c *client) ResolveAPIResource(s string) (*APIResource, error) {
	var gvr schema.GroupVersionResource
	var gvk schema.GroupVersionKind
	var err error

	// Resolve type string into GVR
	fullySpecifiedGVR, gr := schema.ParseResourceArg(strings.ToLower(s))
	if fullySpecifiedGVR != nil {
		gvr, _ = c.mapper.ResourceFor(*fullySpecifiedGVR)
	}
	if gvr.Empty() {
		gvr, err = c.mapper.ResourceFor(gr.WithVersion(""))
		if err != nil {
			if len(gr.Group) == 0 {
				err = fmt.Errorf("the server doesn't have a resource type \"%s\"", gr.Resource)
			} else {
				err = fmt.Errorf("the server doesn't have a resource type \"%s\" in group \"%s\"", gr.Resource, gr.Group)
			}
			return nil, err
		}
	}
	// Obtain Kind from GVR
	gvk, err = c.mapper.KindFor(gvr)
	if gvk.Empty() {
		if err != nil {
			if len(gvr.Group) == 0 {
				err = fmt.Errorf("the server couldn't identify a kind for resource type \"%s\"", gvr.Resource)
			} else {
				err = fmt.Errorf("the server couldn't identify a kind for resource type \"%s\" in group \"%s\"", gvr.Resource, gvr.Group)
			}
			return nil, err
		}
	}
	// Determine scope of resource
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind())
	if err != nil {
		if len(gvk.Group) == 0 {
			err = fmt.Errorf("the server couldn't identify a group kind for resource type \"%s\"", gvk.Kind)
		} else {
			err = fmt.Errorf("the server couldn't identify a group kind for resource type \"%s\" in group \"%s\"", gvk.Kind, gvk.Group)
		}
		return nil, err
	}
	// NOTE: This is a rather incomplete APIResource object, but it has enough
	//       information inside for our use case, which is to fetch API objects
	res := &APIResource{
		Name:       gvr.Resource,
		Namespaced: mapping.Scope.Name() == meta.RESTScopeNameNamespace,
		Group:      gvk.Group,
		Version:    gvk.Version,
		Kind:       gvk.Kind,
	}

	return res, nil
}

func (c *client) Get(ctx context.Context, name string, opts GetOptions) (*unstructuredv1.Unstructured, error) {
	klog.V(4).Infof("Get \"%s\" with options: %+v", name, opts)
	gvr := opts.APIResource.GroupVersionResource()
	var ri dynamic.ResourceInterface
	if opts.APIResource.Namespaced {
		ri = c.dynamicClient.Resource(gvr).Namespace(opts.Namespace)
	} else {
		ri = c.dynamicClient.Resource(gvr)
	}
	return ri.Get(ctx, name, metav1.GetOptions{})
}

//nolint:funlen,gocognit
func (c *client) List(ctx context.Context, opts ListOptions) (*unstructuredv1.UnstructuredList, error) {
	klog.V(4).Infof("List with options: %+v", opts)
	var err error
	apis := opts.APIResources
	if len(apis) == 0 {
		apis, err = c.getAPIResources(ctx)
		if err != nil {
			return nil, err
		}
	}

	// Ensure we're not fetching from duplicated namespaces & determine the scope
	// for listing objects
	isClusterScopeRequest, nsSet := false, make(map[string]struct{})
	if len(opts.Namespaces) == 0 {
		isClusterScopeRequest = true
	}
	for _, ns := range opts.Namespaces {
		if ns != "" {
			nsSet[ns] = struct{}{}
		} else {
			isClusterScopeRequest = true
		}
	}

	var mu sync.Mutex
	var items []unstructuredv1.Unstructured
	createListFn := func(ctx context.Context, api APIResource, ns string) func() error {
		return func() error {
			objects, err := c.listByAPI(ctx, api, ns)
			if err != nil {
				return err
			}
			mu.Lock()
			items = append(items, objects.Items...)
			mu.Unlock()
			return nil
		}
	}
	eg, ctx := errgroup.WithContext(ctx)
	for i := range apis {
		api := apis[i]
		clusterScopeListFn := func() error {
			return createListFn(ctx, api, "")()
		}
		namespaceScopeListFn := func() error {
			egInner, ctxInner := errgroup.WithContext(ctx)
			for ns := range nsSet {
				listFn := createListFn(ctxInner, api, ns)
				egInner.Go(func() error {
					err = listFn()
					// If no permissions to list the resource at the namespace scope,
					// suppress the error to allow other goroutines to continue listing
					if apierrors.IsForbidden(err) {
						err = nil
					}
					return err
				})
			}
			return egInner.Wait()
		}
		eg.Go(func() error {
			var err error
			if isClusterScopeRequest {
				err = clusterScopeListFn()
				// If no permissions to list the cluster-scoped resource,
				// suppress the error to allow other goroutines to continue listing
				if !api.Namespaced && apierrors.IsForbidden(err) {
					err = nil
				}
				// If no permissions to list the namespaced resource at the cluster
				// scope, don't return the error yet & reattempt to list the resource
				// in other namespace(s)
				if !api.Namespaced || !apierrors.IsForbidden(err) {
					return err
				}
			}
			return namespaceScopeListFn()
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	klog.V(4).Infof("Got %4d objects from %d API resources", len(items), len(apis))
	return &unstructuredv1.UnstructuredList{Items: items}, nil
}

// getAPIResources returns all API resources that exists on the cluster.
func (c *client) getAPIResources(_ context.Context) ([]APIResource, error) {
	rls, err := c.discoveryClient.ServerPreferredResources()
	if err != nil {
		return nil, err
	}

	apis := []APIResource{}
	for _, rl := range rls {
		if len(rl.APIResources) == 0 {
			continue
		}
		gv, err := schema.ParseGroupVersion(rl.GroupVersion)
		if err != nil {
			klog.V(4).Infof("Ignoring invalid discovered resource %q: %v", rl.GroupVersion, err)
			continue
		}
		for _, r := range rl.APIResources {
			// Filter resources that can be watched, listed & get
			if len(r.Verbs) == 0 || !sets.NewString(r.Verbs...).HasAll("watch", "list", "get") {
				continue
			}
			api := APIResource{
				Group:      gv.Group,
				Version:    gv.Version,
				Kind:       r.Kind,
				Name:       r.Name,
				Namespaced: r.Namespaced,
			}
			// Exclude duplicated resources (for Kubernetes v1.18 & above)
			switch {
			// migrated to "events.v1.events.k8s.io"
			case api.Group == "" && api.Kind == "Event":
				klog.V(4).Infof("Exclude duplicated discovered resource: %s", api)
				continue
			// migrated to "ingresses.v1.networking.k8s.io"
			case api.Group == "extensions" && api.Kind == "Ingress":
				klog.V(4).Infof("Exclude duplicated discovered resource: %s", api)
				continue
			}
			apis = append(apis, api)
		}
	}

	klog.V(4).Infof("Discovered %d available API resources to list", len(apis))
	return apis, nil
}

// listByAPI list all objects of the provided API & namespace. If listing the
// API at the cluster scope, set the namespace argument as an empty string.
func (c *client) listByAPI(ctx context.Context, api APIResource, ns string) (*unstructuredv1.UnstructuredList, error) {
	var ri dynamic.ResourceInterface
	var items []unstructuredv1.Unstructured
	var next string

	isClusterScopeRequest := !api.Namespaced || ns == ""
	if isClusterScopeRequest {
		ri = c.dynamicClient.Resource(api.GroupVersionResource())
	} else {
		ri = c.dynamicClient.Resource(api.GroupVersionResource()).Namespace(ns)
	}
	for {
		objectList, err := ri.List(ctx, metav1.ListOptions{
			Limit:    250,
			Continue: next,
		})
		if err != nil {
			switch {
			case apierrors.IsForbidden(err):
				if isClusterScopeRequest {
					klog.V(4).Infof("No access to list at cluster scope for resource: %s", api)
				} else {
					klog.V(4).Infof("No access to list in the namespace \"%s\" for resource: %s", ns, api)
				}
				return nil, err
			case apierrors.IsNotFound(err):
				break
			default:
				if isClusterScopeRequest {
					err = fmt.Errorf("failed to list resource type \"%s\" in API group \"%s\" at the cluster scope: %w", api.Name, api.Group, err)
				} else {
					err = fmt.Errorf("failed to list resource type \"%s\" in API group \"%s\" in the namespace \"%s\": %w", api.Name, api.Group, ns, err)
				}
				return nil, err
			}
		}
		if objectList == nil {
			break
		}
		items = append(items, objectList.Items...)
		next = objectList.GetContinue()
		if len(next) == 0 {
			break
		}
	}

	if isClusterScopeRequest {
		klog.V(4).Infof("Got %4d objects from resource at the cluster scope: %s", len(items), api)
	} else {
		klog.V(4).Infof("Got %4d objects from resource in the namespace \"%s\": %s", len(items), ns, api)
	}
	return &unstructuredv1.UnstructuredList{Items: items}, nil
}