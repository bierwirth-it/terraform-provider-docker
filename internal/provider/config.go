package provider

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"

	"context"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"hash/fnv"
	"sort"
)

// Config is the structure that stores the configuration to talk to a
// Docker API compatible host.
type Config struct {
	Host     string
	SSHOpts  []string
	Ca       string
	Cert     string
	Key      string
	CertPath string
}

func NewConfig(d *schema.ResourceData) *Config {
	//log.Println("NewConfig")

	config := Config{
		Host:     "",
		SSHOpts:  make([]string, 0),
		Ca:       "",
		Cert:     "",
		Key:      "",
		CertPath: "",
	}

	// Check for override block and assign values
	if v, ok := d.GetOk("override"); ok {
		if len(v.([]interface{})) > 0 {
			o := v.([]interface{})[0].(map[string]interface{})

			SSHOptsI := o["ssh_opts"].([]interface{})
			SSHOpts := make([]string, len(SSHOptsI))
			for i, s := range SSHOptsI {
				SSHOpts[i] = s.(string)
			}
			config = Config{
				Host:     o["host"].(string),
				SSHOpts:  SSHOpts,
				Ca:       o["ca_material"].(string),
				Cert:     o["cert_material"].(string),
				Key:      o["key_material"].(string),
				CertPath: o["cert_path"].(string),
			}
		}
	}

	return &config
}

func (c *Config) Hash() uint64 {
	var SSHOpts []string

	copy(SSHOpts, c.SSHOpts)
	sort.Strings(SSHOpts)

	hash := fnv.New64()
	_, err := hash.Write([]byte(strings.Join([]string{
		c.Host,
		c.Ca,
		c.Cert,
		c.Key,
		c.CertPath,
		strings.Join(SSHOpts, "|")},
		"|",
	)))
	if err != nil {
		panic(err)
	}

	return hash.Sum64()
}

// buildHTTPClientFromBytes builds the http client from bytes (content of the files)
func buildHTTPClientFromBytes(caPEMCert, certPEMBlock, keyPEMBlock []byte) (*http.Client, error) {
	tlsConfig := &tls.Config{}
	if certPEMBlock != nil && keyPEMBlock != nil {
		tlsCert, err := tls.X509KeyPair(certPEMBlock, keyPEMBlock)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{tlsCert}
	}

	if len(caPEMCert) == 0 {
		tlsConfig.InsecureSkipVerify = true
	} else {
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caPEMCert) {
			return nil, errors.New("could not add RootCA pem")
		}
		tlsConfig.RootCAs = caPool
	}

	tr := defaultTransport()
	tr.TLSClientConfig = tlsConfig
	return &http.Client{Transport: tr}, nil
}

// defaultTransport returns a new http.Transport with similar default values to
// http.DefaultTransport, but with idle connections and keepalive disabled.
func defaultTransport() *http.Transport {
	transport := defaultPooledTransport()
	transport.DisableKeepAlives = true
	transport.MaxIdleConnsPerHost = -1
	return transport
}

// defaultPooledTransport returns a new http.Transport with similar default
// values to http.DefaultTransport.
func defaultPooledTransport() *http.Transport {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   runtime.GOMAXPROCS(0) + 1,
	}
	return transport
}

// NewClient returns a new Docker client.
func (c *Config) NewClient() (*client.Client, error) {
	if c.Cert != "" || c.Key != "" {
		if c.Cert == "" || c.Key == "" {
			return nil, fmt.Errorf("cert_material, and key_material must be specified")
		}

		if c.CertPath != "" {
			return nil, fmt.Errorf("cert_path must not be specified")
		}

		httpClient, err := buildHTTPClientFromBytes([]byte(c.Ca), []byte(c.Cert), []byte(c.Key))
		if err != nil {
			return nil, err
		}

		// Note: don't change the order here, because the custom client
		// needs to be set first them we overwrite the other options: host, version
		return client.NewClientWithOpts(
			client.WithHTTPClient(httpClient),
			client.WithHost(c.Host),
			client.WithAPIVersionNegotiation(),
		)
	}

	if c.CertPath != "" {
		// If there is cert information, load it and use it.
		ca := filepath.Join(c.CertPath, "ca.pem")
		cert := filepath.Join(c.CertPath, "cert.pem")
		key := filepath.Join(c.CertPath, "key.pem")
		return client.NewClientWithOpts(
			client.WithHost(c.Host),
			client.WithTLSClientConfig(ca, cert, key),
			client.WithAPIVersionNegotiation(),
		)
	}

	// If there is no cert information, then check for ssh://
	helper, err := connhelper.GetConnectionHelperWithSSHOpts(c.Host, c.SSHOpts)
	if err != nil {
		return nil, err
	}
	if helper != nil {
		return client.NewClientWithOpts(
			client.WithHost(helper.Host),
			client.WithDialContext(helper.Dialer),
			client.WithAPIVersionNegotiation(),
		)
	}

	// If there is no ssh://, then just return the direct client
	return client.NewClientWithOpts(
		client.WithHost(c.Host),
		client.WithAPIVersionNegotiation(),
	)
}

// Data structure for holding data that we fetch from Docker.
type Data struct {
	DockerImages map[string]*types.ImageSummary
}

// ProviderConfig for the custom registry provider
type ProviderConfig struct {
	// Remove
	// DockerClient *client.Client
	// Remove
	DefaultConfig *Config
	Hosts         map[string]*schema.ResourceData
	AuthConfigs   *AuthConfigs
	clientCache   sync.Map
}

func (c *ProviderConfig) getConfig(d *schema.ResourceData) *Config {
	config := *c.DefaultConfig
	copy(config.SSHOpts, c.DefaultConfig.SSHOpts)

	if d != nil {
		resourceConfig := NewConfig(d)
		if resourceConfig.Host != "" {
			config.Host = resourceConfig.Host
		}
		if len(resourceConfig.SSHOpts) != 0 {
			copy(config.SSHOpts, resourceConfig.SSHOpts)
		}
		if resourceConfig.Ca != "" {
			config.Ca = resourceConfig.Ca
		}
		if resourceConfig.Cert != "" {
			config.Cert = resourceConfig.Cert
		}
		if resourceConfig.Key != "" {
			config.Key = resourceConfig.Key
		}
		if resourceConfig.CertPath != "" {
			config.CertPath = resourceConfig.CertPath
		}
	}
	return &config
}

func (c *ProviderConfig) MakeClient(
	ctx context.Context, d *schema.ResourceData) (*client.Client, error) {
	var dockerClient *client.Client
	var err error

	config := c.getConfig(d)
	configHash := config.Hash()

	cached, found := c.clientCache.Load(configHash)

	if found {
		log.Printf("[DEBUG] Found cached client! Hash:%d Host:%s", configHash, config.Host)

		return cached.(*client.Client), nil
	}
	if config.Cert != "" || config.Key != "" {
		if config.Cert == "" || config.Key == "" {
			return nil, fmt.Errorf("cert_material, and key_material must be specified")
		}

		if config.CertPath != "" {
			return nil, fmt.Errorf("cert_path must not be specified")
		}

		httpClient, err := buildHTTPClientFromBytes([]byte(config.Ca), []byte(config.Cert), []byte(config.Key))
		if err != nil {
			return nil, err
		}

		// Note: don't change the order here, because the custom client
		// needs to be set first them we overwrite the other options: host, version
		dockerClient, _ = client.NewClientWithOpts(
			client.WithHTTPClient(httpClient),
			client.WithHost(config.Host),
			client.WithAPIVersionNegotiation(),
		)
	} else if config.CertPath != "" {
		// If there is cert information, load it and use it.
		ca := filepath.Join(config.CertPath, "ca.pem")
		cert := filepath.Join(config.CertPath, "cert.pem")
		key := filepath.Join(config.CertPath, "key.pem")
		dockerClient, err = client.NewClientWithOpts(
			client.WithHost(config.Host),
			client.WithTLSClientConfig(ca, cert, key),
			client.WithAPIVersionNegotiation(),
		)
	} else if strings.HasPrefix(config.Host, "ssh://") {
		// If there is no cert information, then check for ssh://
		helper, err := connhelper.GetConnectionHelper(config.Host)
		if err != nil {
			return nil, err
		}
		if helper != nil {
			dockerClient, _ = client.NewClientWithOpts(
				client.WithHost(helper.Host),
				client.WithDialContext(helper.Dialer),
				client.WithAPIVersionNegotiation(),
			)
		}
	} else {
		// If there is no ssh://, then just return the direct client
		dockerClient, err = client.NewClientWithOpts(
			client.WithHost(config.Host),
			client.WithAPIVersionNegotiation(),
		)
	}
	if err != nil {
		return nil, err
	}

	c.clientCache.LoadOrStore(configHash, dockerClient)

	_, err = dockerClient.Ping(ctx)
	if err != nil {
		return nil, fmt.Errorf("error pinging Docker server: %s", err)
	}

	log.Printf("[DEBUG] New client with Hash:%d Host:%s", configHash, config.Host)
	return dockerClient, nil
}

// The registry address can be referenced in various places (registry auth, docker config file, image name)
// with or without the http(s):// prefix; this function is used to standardize the inputs
// To support insecure (http) registries, if the address explicitly states "http://" we do not change it.
func normalizeRegistryAddress(address string) string {
	if strings.HasPrefix(address, "http://") {
		return address
	}
	if !strings.HasPrefix(address, "https://") && !strings.HasPrefix(address, "http://") {
		return "https://" + address
	}
	return address
}
