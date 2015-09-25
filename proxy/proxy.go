package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"

	docker "github.com/fsouza/go-dockerclient"
	. "github.com/weaveworks/weave/common"
	weavedocker "github.com/weaveworks/weave/common/docker"
	"github.com/weaveworks/weave/nameserver"
	"github.com/weaveworks/weave/router"
)

const (
	defaultCaFile   = "ca.pem"
	defaultKeyFile  = "key.pem"
	defaultCertFile = "cert.pem"
	dockerSock      = "/var/run/docker.sock"
	dockerSockUnix  = "unix://" + dockerSock
)

var (
	containerCreateRegexp  = dockerAPIEndpoint("containers/create")
	containerStartRegexp   = dockerAPIEndpoint("containers/[^/]*/(re)?start")
	containerInspectRegexp = dockerAPIEndpoint("containers/[^/]*/json")
	execCreateRegexp       = dockerAPIEndpoint("containers/[^/]*/exec")
	execInspectRegexp      = dockerAPIEndpoint("exec/[^/]*/json")

	ErrWeaveCIDRNone = errors.New("the container was created with the '-e WEAVE_CIDR=none' option")
	ErrNoDefaultIPAM = errors.New("the container was created without specifying an IP address with '-e WEAVE_CIDR=...' and the proxy was started with the '--no-default-ipalloc' option")
)

func dockerAPIEndpoint(endpoint string) *regexp.Regexp {
	return regexp.MustCompile("^(/v[0-9\\.]*)?/" + endpoint + "$")
}

type Config struct {
	HostnameFromLabel   string
	HostnameMatch       string
	HostnameReplacement string
	ListenAddrs         []string
	RewriteInspect      bool
	NoDefaultIPAM       bool
	NoRewriteHosts      bool
	TLSConfig           TLSConfig
	Version             string
	WithDNS             bool
	WithoutDNS          bool
}

type wait struct {
	ident string
	ch    chan struct{}
}

type Proxy struct {
	sync.Mutex
	Config
	client              *docker.Client
	dockerBridgeIP      string
	hostnameMatchRegexp *regexp.Regexp
	weaveWaitVolume     string
	normalisedAddrs     []string
	waiters             map[*http.Request]*wait
}

func NewProxy(c Config) (*Proxy, error) {
	p := &Proxy{Config: c, waiters: make(map[*http.Request]*wait)}

	if err := p.TLSConfig.loadCerts(); err != nil {
		Log.Fatalf("Could not configure tls for proxy: %s", err)
	}

	// We pin the protocol version to 1.15 (which corresponds to
	// Docker 1.3.x; the earliest version supported by weave) in order
	// to insulate ourselves from breaking changes to the API, as
	// happened in 1.20 (Docker 1.8.0) when the presentation of
	// volumes changed in `inspect`.
	client, err := weavedocker.NewVersionedClient(dockerSockUnix, "1.15")
	if err != nil {
		return nil, err
	}
	p.client = client.Client

	if !p.WithoutDNS {
		dockerBridgeIP, stderr, err := callWeave("docker-bridge-ip")
		if err != nil {
			return nil, fmt.Errorf(string(stderr))
		}
		p.dockerBridgeIP = string(dockerBridgeIP)
	}

	p.hostnameMatchRegexp, err = regexp.Compile(c.HostnameMatch)
	if err != nil {
		err := fmt.Errorf("Incorrect hostname match '%s': %s", c.HostnameMatch, err.Error())
		return nil, err
	}

	if err = p.findWeaveWaitVolume(); err != nil {
		return nil, err
	}

	client.AddObserver(p)

	return p, nil
}

func (proxy *Proxy) AttachExistingContainers() {
	containers, _ := proxy.client.ListContainers(docker.ListContainersOptions{})
	for _, cont := range containers {
		if strings.HasPrefix(cont.Command, weaveWaitEntrypoint[0]) {
			proxy.ContainerStarted(cont.ID)
		}
	}
}

func (proxy *Proxy) Dial() (net.Conn, error) {
	return net.Dial("unix", dockerSock)
}

func (proxy *Proxy) findWeaveWaitVolume() error {
	container, err := proxy.client.InspectContainer("weaveproxy")
	if err != nil {
		return fmt.Errorf("Could not find the weavewait volume: %s", err)
	}

	if container.Volumes == nil {
		return fmt.Errorf("Could not find the weavewait volume")
	}

	volume, ok := container.Volumes["/w"]
	if !ok {
		return fmt.Errorf("Could not find the weavewait volume")
	}

	proxy.weaveWaitVolume = volume
	return nil
}

func (proxy *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	Log.Infof("%s %s", r.Method, r.URL)
	path := r.URL.Path
	var i interceptor
	switch {
	case containerCreateRegexp.MatchString(path):
		i = &createContainerInterceptor{proxy}
	case containerStartRegexp.MatchString(path):
		i = &startContainerInterceptor{proxy}
	case containerInspectRegexp.MatchString(path):
		i = &inspectContainerInterceptor{proxy}
	case execCreateRegexp.MatchString(path):
		i = &createExecInterceptor{proxy}
	case execInspectRegexp.MatchString(path):
		i = &inspectExecInterceptor{proxy}
	default:
		i = &nullInterceptor{}
	}
	proxy.Intercept(i, w, r)
}

func (proxy *Proxy) Listen() []net.Listener {
	listeners := []net.Listener{}
	proxy.normalisedAddrs = []string{}
	for _, addr := range proxy.ListenAddrs {
		listener, normalisedAddr, err := proxy.listen(addr)
		if err != nil {
			Log.Fatalf("Cannot listen on %s: %s", addr, err)
		}
		listeners = append(listeners, listener)
		proxy.normalisedAddrs = append(proxy.normalisedAddrs, normalisedAddr)
	}

	for _, addr := range proxy.normalisedAddrs {
		Log.Infoln("proxy listening on", addr)
	}
	return listeners
}

func (proxy *Proxy) Serve(listeners []net.Listener) {
	errs := make(chan error)
	for _, listener := range listeners {
		go func(listener net.Listener) {
			errs <- (&http.Server{Handler: proxy}).Serve(listener)
		}(listener)
	}
	for range listeners {
		err := <-errs
		if err != nil {
			Log.Fatalf("Serve failed: %s", err)
		}
	}
}

func (proxy *Proxy) ListenAndServeStatus(socket string) {
	listener, err := net.Listen("unix", socket)
	if err != nil {
		Log.Fatalf("ListenAndServeStatus failed: %s", err)
	}
	handler := http.HandlerFunc(proxy.StatusHTTP)
	if err := (&http.Server{Handler: handler}).Serve(listener); err != nil {
		Log.Fatalf("ListenAndServeStatus failed: %s", err)
	}
}

func (proxy *Proxy) StatusHTTP(w http.ResponseWriter, r *http.Request) {
	for _, addr := range proxy.normalisedAddrs {
		fmt.Fprintln(w, addr)
	}
}

func copyOwnerAndPermissions(from, to string) error {
	stat, err := os.Stat(from)
	if err != nil {
		return err
	}
	if err = os.Chmod(to, stat.Mode()); err != nil {
		return err
	}

	moreStat, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}

	if err = os.Chown(to, int(moreStat.Uid), int(moreStat.Gid)); err != nil {
		return err
	}

	return nil
}

func (proxy *Proxy) listen(protoAndAddr string) (net.Listener, string, error) {
	var (
		listener    net.Listener
		err         error
		proto, addr string
	)

	if protoAddrParts := strings.SplitN(protoAndAddr, "://", 2); len(protoAddrParts) == 2 {
		proto, addr = protoAddrParts[0], protoAddrParts[1]
	} else if strings.HasPrefix(protoAndAddr, "/") {
		proto, addr = "unix", protoAndAddr
	} else {
		proto, addr = "tcp", protoAndAddr
	}

	switch proto {
	case "tcp":
		listener, err = net.Listen(proto, addr)
		if err != nil {
			return nil, "", err
		}
		if proxy.TLSConfig.enabled() {
			listener = tls.NewListener(listener, proxy.TLSConfig.Config)
		}

	case "unix":
		// remove socket from last invocation
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			return nil, "", err
		}
		listener, err = net.Listen(proto, addr)
		if err != nil {
			return nil, "", err
		}
		if err = copyOwnerAndPermissions(dockerSock, addr); err != nil {
			return nil, "", err
		}

	default:
		Log.Fatalf("Invalid protocol format: %q", proto)
	}

	return listener, fmt.Sprintf("%s://%s", proto, addr), nil
}

// weavedocker.ContainerObserver interface
func (proxy *Proxy) ContainerStarted(ident string) {
	container, err := proxy.client.InspectContainer(ident)
	if err != nil {
		Log.Warningf("Error inspecting container %s: %v", ident, err)
		return
	}
	// If this was a container we modified the entrypoint for, attach it to the network
	if containerShouldAttach(container) {
		proxy.attach(container)
	}
	proxy.notifyWaiters(container.ID)
}

func containerShouldAttach(container *docker.Container) bool {
	return len(container.Config.Entrypoint) > 0 && container.Config.Entrypoint[0] == weaveWaitEntrypoint[0]
}

func (proxy *Proxy) createWait(r *http.Request, ident string) {
	proxy.Lock()
	ch := make(chan struct{})
	proxy.waiters[r] = &wait{ident: ident, ch: ch}
	proxy.Unlock()
}

func (proxy *Proxy) removeWait(r *http.Request) {
	proxy.Lock()
	delete(proxy.waiters, r)
	proxy.Unlock()
}

func (proxy *Proxy) notifyWaiters(ident string) {
	proxy.Lock()
	for _, wait := range proxy.waiters {
		if ident == wait.ident && wait.ch != nil {
			close(wait.ch)
			wait.ch = nil
		}
	}
	proxy.Unlock()
}

func (proxy *Proxy) waitForStart(r *http.Request) {
	var ch chan struct{}
	proxy.Lock()
	wait, found := proxy.waiters[r]
	if found {
		ch = wait.ch
	}
	proxy.Unlock()
	if ch != nil {
		Log.Debugf("Wait for start of container %s", wait.ident)
		<-ch
	}
}

func (proxy *Proxy) ContainerDied(ident string) {
}

func (proxy *Proxy) attach(container *docker.Container) error {
	cidrs, err := proxy.weaveCIDRsFromConfig(container.Config, container.HostConfig)
	if err != nil {
		Log.Infof("Leaving container %s alone because %s", container.ID, err)
		return nil
	}
	Log.Infof("Attaching container %s with WEAVE_CIDR \"%s\" to weave network", container.ID, strings.Join(cidrs, " "))
	args := []string{"attach"}
	args = append(args, cidrs...)
	if !proxy.NoRewriteHosts {
		args = append(args, "--rewrite-hosts")
	}
	args = append(args, "--or-die", container.ID)
	if _, stderr, err := callWeave(args...); err != nil {
		Log.Warningf("Attaching container %s to weave network failed: %s", container.ID, string(stderr))
		return errors.New(string(stderr))
	} else if len(stderr) > 0 {
		Log.Warningf("Attaching container %s to weave network: %s", container.ID, string(stderr))
	}

	return nil
}

func (proxy *Proxy) weaveCIDRsFromConfig(config *docker.Config, hostConfig *docker.HostConfig) ([]string, error) {
	netMode := ""
	if hostConfig != nil {
		netMode = hostConfig.NetworkMode
	}
	if netMode == "host" || strings.HasPrefix(netMode, "container:") {
		return nil, fmt.Errorf("the container was created with the '--net=%s'", netMode)
	}
	for _, e := range config.Env {
		if strings.HasPrefix(e, "WEAVE_CIDR=") {
			if e[11:] == "none" {
				return nil, ErrWeaveCIDRNone
			}
			return strings.Fields(e[11:]), nil
		}
	}
	if proxy.NoDefaultIPAM {
		return nil, ErrNoDefaultIPAM
	}
	return nil, nil
}

func (proxy *Proxy) addWeaveWaitVolume(hostConfig *docker.HostConfig) {
	var binds []string
	for _, bind := range hostConfig.Binds {
		s := strings.Split(bind, ":")
		if len(s) >= 2 && s[1] == "/w" {
			continue
		}
		binds = append(binds, bind)
	}
	hostConfig.Binds = append(binds, fmt.Sprintf("%s:/w:ro", proxy.weaveWaitVolume))
}

func (proxy *Proxy) setWeaveDNS(hostConfig *docker.HostConfig, hostname, dnsDomain string) error {
	hostConfig.DNS = append(hostConfig.DNS, proxy.dockerBridgeIP)

	if len(hostConfig.DNSSearch) == 0 {
		if hostname == "" {
			hostConfig.DNSSearch = []string{dnsDomain}
		} else {
			hostConfig.DNSSearch = []string{"."}
		}
	}

	return nil
}

func (proxy *Proxy) getDNSDomain() (domain string) {
	if proxy.WithoutDNS {
		return ""
	}

	if proxy.WithDNS {
		domain = nameserver.DefaultDomain
	}

	weaveContainer, err := proxy.client.InspectContainer("weave")
	if err != nil ||
		weaveContainer.NetworkSettings == nil ||
		weaveContainer.NetworkSettings.IPAddress == "" {
		return
	}

	url := fmt.Sprintf("http://%s:%d/domain", weaveContainer.NetworkSettings.IPAddress, router.HTTPPort)
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	return string(b)
}
