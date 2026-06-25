package applier

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/cobr-io/flywheel/internal/cli/style"
)

// PortForward holds a live port-forward to a pod's container port.
// Close() stops the forward.
type PortForward struct {
	LocalPort uint16
	stop      chan struct{}
	done      chan struct{}
}

func (p *PortForward) Close() {
	close(p.stop)
	<-p.done
}

// ForwardToService picks the first Ready pod backing `svc` (in
// `namespace`) and forwards a local port to the pod's `targetPort`.
// Returns a PortForward whose LocalPort is the OS-chosen ephemeral port.
//
// Used by `flywheel up` step 11c to push the cached Flywheel clone into
// the in-cluster git-server before Flux reconciles from the mirror.
func ForwardToService(ctx context.Context, kubeconfigPath, contextName, namespace, svcName string, targetPort int, out io.Writer) (*PortForward, error) {
	cfg, err := loadRESTConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	// Find a Ready pod backing the service. Service selector → pod
	// label match.
	svc, err := cs.CoreV1().Services(namespace).Get(ctx, svcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get service %s/%s: %w", namespace, svcName, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return nil, fmt.Errorf("service %s/%s has no selector", namespace, svcName)
	}
	selector := labelSelector(svc.Spec.Selector)
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		if isReady(&pods.Items[i]) {
			pod = &pods.Items[i]
			break
		}
	}
	if pod == nil {
		return nil, fmt.Errorf("no Ready pod backing %s/%s yet", namespace, svcName)
	}

	// Build a SPDY round-tripper for the port-forward subresource.
	url := portForwardURL(cfg.Host, namespace, pod.Name)
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return nil, err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport},
		http.MethodPost, url)

	stop := make(chan struct{})
	ready := make(chan struct{})
	done := make(chan struct{})

	// Local port :0 → OS-picked free port. Route the port-forward's
	// own chatter ("Forwarding from … -> N", "Handling connection
	// for N") through VerboseWriter so it stays quiet unless -v.
	pfOut := style.VerboseWriter(out)
	pf, err := portforward.New(dialer,
		[]string{fmt.Sprintf("0:%d", targetPort)},
		stop, ready, pfOut, pfOut)
	if err != nil {
		return nil, err
	}

	go func() {
		defer close(done)
		_ = pf.ForwardPorts()
	}()

	select {
	case <-ready:
	case <-ctx.Done():
		close(stop)
		<-done
		return nil, ctx.Err()
	case <-time.After(20 * time.Second):
		close(stop)
		<-done
		return nil, fmt.Errorf("port-forward to %s/%s never became ready", namespace, svcName)
	}

	ports, err := pf.GetPorts()
	if err != nil {
		close(stop)
		<-done
		return nil, err
	}
	if len(ports) == 0 {
		close(stop)
		<-done
		return nil, fmt.Errorf("port-forward returned no ports")
	}
	return &PortForward{
		LocalPort: ports[0].Local,
		stop:      stop,
		done:      done,
	}, nil
}

func portForwardURL(host, namespace, pod string) *url.URL {
	u, _ := url.Parse(host)
	u.Path = fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", namespace, pod)
	return u
}

// WaitForServiceReady polls the service's endpoints until at least one
// is ready. Used before ForwardToService when the caller knows the
// resource was just applied.
func WaitForServiceReady(ctx context.Context, kubeconfigPath, contextName, namespace, svcName string, timeout time.Duration) error {
	cfg, err := loadRESTConfig(kubeconfigPath, contextName)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		eps, err := cs.CoreV1().Endpoints(namespace).Get(ctx, svcName, metav1.GetOptions{})
		if err == nil {
			for _, ss := range eps.Subsets {
				if len(ss.Addresses) > 0 {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("service %s/%s never got a Ready endpoint within %s", namespace, svcName, timeout)
}

func labelSelector(m map[string]string) string {
	first := true
	out := ""
	for k, v := range m {
		if !first {
			out += ","
		}
		out += fmt.Sprintf("%s=%s", k, v)
		first = false
	}
	return out
}

func isReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
