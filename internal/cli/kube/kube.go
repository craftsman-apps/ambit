// Package kube holds the CLI's Kubernetes helpers: clients for one kubeconfig context, confirmation, server-side
// apply, waiting with progress, and an HTTP probe through a port-forward.
package kube

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	ambitv1alpha1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// GatewaySelector selects the gateway Deployment and pods, which agentgateway's controller creates for the Gateway "ambit".
const GatewaySelector = "gateway.networking.k8s.io/gateway-name=ambit"

// Target selects a cluster. Empty Kubeconfig: $KUBECONFIG or ~/.kube/config. Empty Context: the current context.
// Log receives what client-go, controller-runtime and klog would print (server warnings, library logs); nil discards it.
type Target struct {
	Kubeconfig, Context string
	Log                 io.Writer
}

// Clients talk to one context. Context and Server are what the CLI prints before it changes anything.
type Clients struct {
	Config  *rest.Config
	Kube    kubernetes.Interface
	Ctrl    client.Client
	Context string
	Server  string
}

// Scheme knows the core types, CRDs and Ambit's objects.
var Scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, apiextensionsv1.AddToScheme, ambitv1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			panic(err)
		}
	}
	return s
}()

// logSink is where klog writes. klog and controller-runtime are process-wide, so they are set up once and follow
// the Log of the latest Connect.
var logSink struct {
	sync.Mutex
	w io.Writer
}

type sinkWriter struct{}

func (sinkWriter) Write(b []byte) (int, error) {
	logSink.Lock()
	defer logSink.Unlock()
	return logSink.w.Write(b)
}

var routeLibraryLogs sync.Once

// useLog sends klog and controller-runtime output to w, and nothing to stderr.
func useLog(w io.Writer) {
	logSink.Lock()
	logSink.w = w
	logSink.Unlock()
	routeLibraryLogs.Do(func() {
		klog.LogToStderr(false)
		klog.SetOutput(sinkWriter{})
		ctrllog.SetLogger(klog.NewKlogr())
	})
}

// Connect builds clients for t. It does not contact the cluster.
func Connect(t Target) (*Clients, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if t.Kubeconfig != "" {
		rules.ExplicitPath = t.Kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: t.Context})
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, err
	}
	name := t.Context
	if name == "" {
		name = raw.CurrentContext
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 30 * time.Second
	log := t.Log
	if log == nil {
		log = io.Discard
	}
	useLog(log)
	cfg.WarningHandler = rest.NewWarningWriter(log, rest.WarningWriterOptions{})
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	cl, err := client.New(cfg, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, err
	}
	return &Clients{Config: cfg, Kube: cs, Ctrl: cl, Context: name, Server: cfg.Host}, nil
}

// Confirm prints the lines that describe the target, then asks question and waits for "y". With yes it does not
// ask. Without a terminal (interactive false) and without yes it refuses with a usage error.
func Confirm(p *out.Printer, in io.Reader, interactive, yes bool, lines []string, question string) error {
	for _, l := range lines {
		p.Line("%s", l)
	}
	if yes {
		return nil
	}
	if !interactive {
		return out.Usage("no confirmation", "there is no terminal to ask in", "run the command again with --yes")
	}
	p.Line("%s [y/N]", question)
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		return &out.Error{What: "cancelled", Why: fmt.Sprintf("the answer was %q", strings.TrimSpace(answer)), Code: out.ExitFailed}
	}
	return nil
}
