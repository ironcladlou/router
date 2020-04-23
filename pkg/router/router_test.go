package router_test

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"testing"
	"time"

	routev1 "github.com/openshift/api/route/v1"

	projectfake "github.com/openshift/client-go/project/clientset/versioned/fake"
	routeclient "github.com/openshift/client-go/route/clientset/versioned"
	routefake "github.com/openshift/client-go/route/clientset/versioned/fake"
	routelisters "github.com/openshift/client-go/route/listers/route/v1"

	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	kubefake "k8s.io/client-go/kubernetes/fake"

	"k8s.io/klog"

	routercmd "github.com/openshift/router/pkg/cmd/infra/router"
	"github.com/openshift/router/pkg/router"
	"github.com/openshift/router/pkg/router/controller"
	templateplugin "github.com/openshift/router/pkg/router/template"
	"github.com/openshift/router/pkg/router/writerlease"
)

type harness struct {
	routeClient routeclient.Interface

	namespace string
	uidCount  int
}

func (h *harness) nextUID() types.UID {
	h.uidCount++
	return types.UID(fmt.Sprintf("%03d", h.uidCount))
}

var h *harness

func TestMain(m *testing.M) {
	logFlags := flag.FlagSet{}
	klog.InitFlags(&logFlags)
	if err := logFlags.Set("v", "6"); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Client stuff
	client := kubefake.NewSimpleClientset()
	routeClient := routefake.NewSimpleClientset()
	projectClient := projectfake.NewSimpleClientset()

	namespace := "default"

	h = &harness{
		routeClient: routeClient,
		namespace:   namespace,
	}

	// Other shared junk
	routerSelection := &routercmd.RouterSelection{}
	factory := routerSelection.NewFactory(routeClient, projectClient.ProjectV1().Projects(), client)
	informer := factory.CreateRoutesSharedInformer()
	routeLister := routelisters.NewRouteLister(informer.GetIndexer())
	lease := writerlease.New(time.Minute, 3*time.Second)
	go lease.Run(wait.NeverStop)
	tracker := controller.NewSimpleContentionTracker(informer, namespace, 60*time.Second)
	tracker.SetConflictMessage(fmt.Sprintf("The router detected another process is writing conflicting updates to route status with name %q. Please ensure that the configuration of all routers is consistent. Route status will not be updated as long as conflicts are detected.", namespace))
	go tracker.Run(wait.NeverStop)

	var plugin router.Plugin

	workdir, err := ioutil.TempDir("", "router")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("router working directory: %s\n", workdir)

	// The template plugin which is wrapped
	svcFetcher := templateplugin.NewListWatchServiceLookup(client.CoreV1(), 60*time.Second, namespace)
	pluginCfg := templateplugin.TemplatePluginConfig{
		WorkingDir:            workdir,
		DefaultCertificateDir: workdir,
		ReloadFn:              func(shutdown bool) error { return nil },
		TemplatePath:          "../../images/router/haproxy/conf/haproxy-config.template",
		ReloadInterval:        15 * time.Second,
	}
	plugin, err = templateplugin.NewTemplatePlugin(pluginCfg, svcFetcher)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Wrap the template plugin with other stuff
	statusPlugin := controller.NewStatusAdmitter(plugin, routeClient.RouteV1(), routeLister, "default", "example.com", lease, tracker)
	plugin = statusPlugin
	plugin = controller.NewUniqueHost(plugin, routerSelection.DisableNamespaceOwnershipCheck, statusPlugin)
	plugin = controller.NewHostAdmitter(plugin, routerSelection.RouteAdmissionFunc(), false, false, statusPlugin)

	// Start the controller
	c := factory.Create(plugin, false)
	c.Run()

	os.Exit(m.Run())
}

func TestAdmissionEdgeCases(t *testing.T) {
	start := time.Now()

	tests := map[string][]expectation{
		"deletion promotes inactive routes": {
			mustCreate{name: "a", host: "example.com", path: "", time: start},
			mustCreate{name: "b", host: "example.com", path: "/foo", time: start.Add(1 * time.Minute)},
			mustCreate{name: "c", host: "example.com", path: "/foo", time: start.Add(2 * time.Minute)},
			mustCreate{name: "d", host: "example.com", path: "/foo", time: start.Add(3 * time.Minute)},
			mustCreate{name: "e", host: "example.com", path: "/bar", time: start.Add(4 * time.Minute)},

			expectAdmitted{"a", "b", "e"},
			expectRejected{"c", "d"},

			mustDelete{"b"},

			expectAdmitted{"a", "c", "e"},
			expectRejected{"d"},

			mustDelete{"c"},

			expectAdmitted{"a", "d", "e"},
			expectRejected{},

			mustDelete{"e"},

			expectAdmitted{"a", "d"},
			expectRejected{},
		},
	}

	for name, expectations := range tests {
		for _, expectation := range expectations {
			err := expectation.Apply(h)
			if err != nil {
				t.Fatalf("%s failed: %v", name, err)
			}
		}
	}
}

type expectation interface {
	Apply(h *harness) error
}

type mustCreate struct {
	name string
	host string
	path string
	time time.Time
}

func (e mustCreate) Apply(h *harness) error {
	route := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.Time{Time: e.time},
			Namespace:         h.namespace,
			Name:              e.name,
			UID:               h.nextUID(),
		},
		Spec: routev1.RouteSpec{
			Host: e.host,
			Path: e.path,
			To: routev1.RouteTargetReference{
				Name:   "service" + e.name,
				Weight: new(int32),
			},
			WildcardPolicy: routev1.WildcardPolicyNone,
		},
	}
	_, err := h.routeClient.RouteV1().Routes(route.Namespace).Create(context.TODO(), route, metav1.CreateOptions{})
	return err
}

type mustDelete []string

func (e mustDelete) Apply(h *harness) error {
	for _, name := range e {
		if err := h.routeClient.RouteV1().Routes(h.namespace).Delete(context.TODO(), name, metav1.DeleteOptions{}); err != nil {
			return err
		}
	}
	return nil
}

type expectAdmitted []string

func (e expectAdmitted) Apply(h *harness) error {
	for i := range e {
		if err := assertAdmitted(h, e[i], true); err != nil {
			return err
		}
	}
	return nil
}

type expectRejected []string

func (e expectRejected) Apply(h *harness) error {
	for i := range e {
		if err := assertAdmitted(h, e[i], false); err != nil {
			return err
		}
	}
	return nil
}

func assertAdmitted(h *harness, name string, admitted bool) error {
	err := wait.PollImmediate(1*time.Second, 10*time.Second, func() (bool, error) {
		route, err := h.routeClient.RouteV1().Routes(h.namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if len(route.Status.Ingress) == 0 || len(route.Status.Ingress[0].Conditions) == 0 {
			return false, nil
		}
		cond := route.Status.Ingress[0].Conditions[0]
		var expected corev1.ConditionStatus
		if admitted {
			expected = corev1.ConditionTrue
		} else {
			expected = corev1.ConditionFalse
		}
		done := cond.Type == routev1.RouteAdmitted && cond.Status == expected
		return done, nil
	})
	if err != nil {
		return fmt.Errorf("timed out waiting for route %s/%s to be admitted=%v", h.namespace, name, admitted)
	}
	return nil
}
