package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"
)

const (
	requestTimeout     = 20 * time.Second
	listPageLimit      = "500"
	maxConcurrent      = 8
	defaultResourceKey = "pod"
)

type kubeConfig struct {
	Clusters       []namedCluster `yaml:"clusters"`
	Contexts       []namedContext `yaml:"contexts"`
	Users          []namedUser    `yaml:"users"`
	CurrentContext string         `yaml:"current-context"`
}

type namedCluster struct {
	Name    string      `yaml:"name"`
	Cluster clusterInfo `yaml:"cluster"`
}

type clusterInfo struct {
	Server                   string `yaml:"server"`
	CertificateAuthority     string `yaml:"certificate-authority"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
	InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
	TLSServerName            string `yaml:"tls-server-name"`
}

type namedContext struct {
	Name    string      `yaml:"name"`
	Context contextInfo `yaml:"context"`
}

type contextInfo struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

type namedUser struct {
	Name string   `yaml:"name"`
	User userInfo `yaml:"user"`
}

type userInfo struct {
	Token                 string      `yaml:"token"`
	TokenFile             string      `yaml:"tokenFile"`
	Username              string      `yaml:"username"`
	Password              string      `yaml:"password"`
	ClientCertificate     string      `yaml:"client-certificate"`
	ClientCertificateData string      `yaml:"client-certificate-data"`
	ClientKey             string      `yaml:"client-key"`
	ClientKeyData         string      `yaml:"client-key-data"`
	Exec                  *execConfig `yaml:"exec"`
}

type execConfig struct {
	APIVersion string         `yaml:"apiVersion"`
	Command    string         `yaml:"command"`
	Args       []string       `yaml:"args"`
	Env        []execEnvEntry `yaml:"env"`
}

type execEnvEntry struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type clusterTarget struct {
	name    string
	cluster clusterInfo
	user    userInfo
	baseDir string
}

type clusterResult struct {
	Name           string
	Namespaces     []string
	ResourceCounts map[string]int
	PodImageCounts map[string]map[string]int
	Resources      map[string][]discoveredResource
	Err            error
}

type namespaceItem struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

type namespacedObjectItem struct {
	Metadata struct {
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Containers          []containerImage `json:"containers"`
		InitContainers      []containerImage `json:"initContainers"`
		EphemeralContainers []containerImage `json:"ephemeralContainers"`
	} `json:"spec"`
}

type containerImage struct {
	Image string `json:"image"`
}

type resourceCountResult struct {
	Counts         map[string]int
	PodImageCounts map[string]map[string]int
}

type apiVersionList struct {
	Versions []string `json:"versions"`
}

type apiGroupList struct {
	Groups []struct {
		Name             string `json:"name"`
		PreferredVersion struct {
			GroupVersion string `json:"groupVersion"`
		} `json:"preferredVersion"`
		Versions []struct {
			GroupVersion string `json:"groupVersion"`
		} `json:"versions"`
	} `json:"groups"`
}

type apiResourceList struct {
	GroupVersion string `json:"groupVersion"`
	Resources    []struct {
		Name       string   `json:"name"`
		Kind       string   `json:"kind"`
		Namespaced bool     `json:"namespaced"`
		Verbs      []string `json:"verbs"`
	} `json:"resources"`
}

type discoveredResource struct {
	Kind         string
	GroupVersion string
	Name         string
}

type resourceKindOption struct {
	Key   string
	Label string
}

type resourceList[T any] struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []T `json:"items"`
}

type pageData struct {
	Clusters         []clusterResult
	Namespaces       []string
	ResourceKinds    []resourceKindOption
	SelectedResource string
	SelectedClusters map[int]bool
	HideEmpty        bool
}

//go:embed index.html styles.css app.js
var webAssets embed.FS

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve home directory: %v\n", err)
		os.Exit(1)
	}
	configPath := flag.String("kubeconfig", envOrDefault("KUBECONFIG", filepath.Join(home, ".kube", "config")), "path to kubeconfig")
	address := flag.String("addr", envOrDefault("ADDR", "127.0.0.1:8080"), "http listen address")
	excludeClustersPattern := flag.String("exclude-clusters", envOrDefault("EXCLUDE_CLUSTERS", ""), "regular expression matching kubeconfig cluster names to exclude")
	flag.Parse()

	var excludeClusters *regexp.Regexp
	if *excludeClustersPattern != "" {
		excludeClusters, err = regexp.Compile(*excludeClustersPattern)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid -exclude-clusters regular expression: %v\n", err)
			os.Exit(1)
		}
	}

	targets, err := loadTargets(*configPath, excludeClusters)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              *address,
		Handler:           newHandler(targets),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("Serving namespace grid at http://%s/\n", *address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newHandler(targets []clusterTarget) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/styles.css", serveWebAsset("styles.css", "text/css; charset=utf-8"))
	mux.HandleFunc("/app.js", serveWebAsset("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		selectedResource := strings.ToLower(r.URL.Query().Get("kind"))
		if selectedResource == "" {
			selectedResource = defaultResourceKey
		}
		hideEmpty := r.URL.Query().Get("hide_empty") == "1"
		clusterNames := make([]string, len(targets))
		for index, target := range targets {
			clusterNames[index] = target.name
		}
		selectedClusters, err := selectedClusterIndexes(r.URL.RawQuery, clusterNames)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pageTemplate, err := loadPageTemplate()
		if err != nil {
			fmt.Fprintf(os.Stderr, "load page template: %v\n", err)
			http.Error(w, "could not load page template", http.StatusInternalServerError)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		results := queryClusters(ctx, targets, selectedResource)
		data := buildPageData(results, selectedResource, selectedClusters)
		data.HideEmpty = hideEmpty
		if !slices.ContainsFunc(data.ResourceKinds, func(option resourceKindOption) bool {
			return option.Key == selectedResource
		}) {
			http.Error(w, "unknown resource kind", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if err := pageTemplate.Execute(w, data); err != nil {
			fmt.Fprintf(os.Stderr, "render HTML: %v\n", err)
		}
	})
	return mux
}

func serveWebAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		content, err := loadWebAsset(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load web asset %q: %v\n", name, err)
			http.Error(w, "could not load web asset", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		if _, err := w.Write(content); err != nil {
			fmt.Fprintf(os.Stderr, "write web asset %q response: %v\n", name, err)
		}
	}
}

func loadWebAsset(name string) ([]byte, error) {
	content, err := os.ReadFile(name)
	if err == nil {
		return content, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read disk asset %q: %w", name, err)
	}
	content, err = webAssets.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read embedded asset %q: %w", name, err)
	}
	return content, nil
}

func loadPageTemplate() (*template.Template, error) {
	content, err := loadWebAsset("index.html")
	if err != nil {
		return nil, err
	}
	return template.New("index.html").Funcs(template.FuncMap{
		"contains": func(namespaces []string, namespace string) bool { return slices.Contains(namespaces, namespace) },
		"hasDifference": func(namespace string, clusters []clusterResult, selected map[int]bool) bool {
			hasPresent, hasMissing := false, false
			for index, cluster := range clusters {
				if !selected[index] || cluster.Err != nil {
					continue
				}
				if slices.Contains(cluster.Namespaces, namespace) {
					hasPresent = true
				} else {
					hasMissing = true
				}
			}
			return hasPresent && hasMissing
		},
		"clusterTooltip": clusterTooltip,
		"len":            func(values []clusterResult) int { return len(values) },
		"add":            func(left, right int) int { return left + right },
	}).Parse(string(content))
}

func loadTargets(path string, excludeClusters *regexp.Regexp) ([]clusterTarget, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig %q: %w", path, err)
	}
	var config kubeConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse kubeconfig %q: %w", path, err)
	}

	users := make(map[string]userInfo, len(config.Users))
	for _, entry := range config.Users {
		users[entry.Name] = entry.User
	}
	contexts := make(map[string][]namedContext)
	contextByName := make(map[string]namedContext, len(config.Contexts))
	for _, entry := range config.Contexts {
		contexts[entry.Context.Cluster] = append(contexts[entry.Context.Cluster], entry)
		contextByName[entry.Name] = entry
	}
	currentContext := contextByName[config.CurrentContext]

	targets := make([]clusterTarget, 0, len(config.Clusters))
	excluded := 0
	for _, entry := range config.Clusters {
		if excludeClusters != nil && excludeClusters.MatchString(entry.Name) {
			excluded++
			continue
		}
		parsed, err := url.Parse(entry.Cluster.Server)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return nil, fmt.Errorf("cluster %q has invalid API server URL %q", entry.Name, entry.Cluster.Server)
		}
		target := clusterTarget{name: entry.Name, cluster: entry.Cluster, baseDir: filepath.Dir(path)}
		matchingContexts := contexts[entry.Name]
		sort.Slice(matchingContexts, func(i, j int) bool {
			return matchingContexts[i].Name < matchingContexts[j].Name
		})
		selectedContext := namedContext{}
		for _, contextEntry := range matchingContexts {
			if contextEntry.Name == config.CurrentContext {
				selectedContext = contextEntry
				break
			}
		}
		if selectedContext.Name == "" && len(matchingContexts) > 0 {
			selectedContext = matchingContexts[0]
		}
		if currentContext.Name != "" && currentContext.Context.Cluster == entry.Name {
			selectedContext = currentContext
		}
		target.user = users[selectedContext.Context.User]
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		if excluded > 0 {
			return nil, errors.New("all clusters were excluded by -exclude-clusters")
		}
		return nil, errors.New("kubeconfig contains no clusters")
	}
	return targets, nil
}

func queryClusters(ctx context.Context, targets []clusterTarget, selectedResource string) []clusterResult {
	results := make([]clusterResult, len(targets))
	semaphore := make(chan struct{}, maxConcurrent)
	var wait sync.WaitGroup
	for i, target := range targets {
		wait.Add(1)
		go func(i int, target clusterTarget) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[i] = clusterResult{Name: target.name, Err: ctx.Err()}
				return
			}
			client, headers, err := makeHttpClient(ctx, target)
			if err != nil {
				results[i] = clusterResult{Name: target.name, Err: fmt.Errorf("credentials: %w", err)}
				return
			}
			namespaces, namespaceErr := listNamespaces(ctx, target, client, headers)
			resources, discoveryErr := discoverNamespacedResources(ctx, target, client, headers)
			if namespaceErr != nil || discoveryErr != nil {
				results[i] = clusterResult{
					Name:       target.name,
					Namespaces: namespaces,
					Resources:  resources,
					Err:        errors.Join(namespaceErr, discoveryErr),
				}
				return
			}
			counts := make(map[string]int)
			podImageCounts := make(map[string]map[string]int)
			if selectedResources, ok := resources[selectedResource]; ok {
				countResult, countErr := listResourceCounts(ctx, target, client, headers, selectedResources)
				err = countErr
				if err != nil {
					results[i] = clusterResult{Name: target.name, Namespaces: namespaces, Resources: resources, Err: err}
					return
				}
				counts = countResult.Counts
				podImageCounts = countResult.PodImageCounts
			}
			results[i] = clusterResult{
				Name:           target.name,
				Namespaces:     namespaces,
				ResourceCounts: counts,
				PodImageCounts: podImageCounts,
				Resources:      resources,
			}
		}(i, target)
	}
	wait.Wait()
	return results
}

func listNamespaces(ctx context.Context, target clusterTarget, client *http.Client, headers http.Header) ([]string, error) {
	items, err := listResourceItems[namespaceItem](ctx, target, client, headers, "/api/v1/namespaces", "namespaces")
	if err != nil {
		return nil, err
	}
	namespaces := make([]string, 0, len(items))
	for _, item := range items {
		namespaces = append(namespaces, item.Metadata.Name)
	}
	sort.Strings(namespaces)
	return namespaces, nil
}

func listResourceCounts(ctx context.Context, target clusterTarget, client *http.Client, headers http.Header, resources []discoveredResource) (resourceCountResult, error) {
	result := resourceCountResult{
		Counts:         make(map[string]int),
		PodImageCounts: make(map[string]map[string]int),
	}
	for _, resource := range resources {
		path := apiGroupVersionPath(resource.GroupVersion) + "/" + resource.Name
		items, err := listResourceItems[namespacedObjectItem](ctx, target, client, headers, path, resource.Kind+" ("+resource.GroupVersion+")")
		if err != nil {
			return resourceCountResult{}, err
		}
		for _, item := range items {
			namespace := item.Metadata.Namespace
			result.Counts[namespace]++
			if !strings.EqualFold(resource.Kind, "Pod") {
				continue
			}
			imagesInPod := make(map[string]struct{})
			for _, container := range item.Spec.Containers {
				if container.Image != "" {
					imagesInPod[container.Image] = struct{}{}
				}
			}
			for _, container := range item.Spec.InitContainers {
				if container.Image != "" {
					imagesInPod[container.Image] = struct{}{}
				}
			}
			for _, container := range item.Spec.EphemeralContainers {
				if container.Image != "" {
					imagesInPod[container.Image] = struct{}{}
				}
			}
			for image := range imagesInPod {
				if result.PodImageCounts[namespace] == nil {
					result.PodImageCounts[namespace] = make(map[string]int)
				}
				result.PodImageCounts[namespace][image]++
			}
		}
	}
	return result, nil
}

func clusterTooltip(cluster clusterResult, namespace, selectedResource string) string {
	lines := []string{cluster.Name}
	if selectedResource != defaultResourceKey {
		return strings.Join(lines, "\n")
	}
	images := cluster.PodImageCounts[namespace]
	imageNames := make([]string, 0, len(images))
	for image := range images {
		imageNames = append(imageNames, image)
	}
	sort.Strings(imageNames)
	for _, image := range imageNames {
		if images[image] > 1 {
			lines = append(lines, fmt.Sprintf("%s (%d)", image, images[image]))
		} else {
			lines = append(lines, image)
		}
	}
	return strings.Join(lines, "\n")
}

func listResourceItems[T any](ctx context.Context, target clusterTarget, client *http.Client, headers http.Header, path, resource string) ([]T, error) {
	endpoint := strings.TrimRight(target.cluster.Server, "/") + path
	requestURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	continueToken := ""
	var items []T
	for {
		query := requestURL.Query()
		query.Set("limit", listPageLimit)
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		requestURL.RawQuery = query.Encode()
		response, err := apiDoRequest(ctx, client, headers, requestURL, resource)
		if err != nil {
			return nil, err
		}
		var page resourceList[T]
		if err := decodeAPIResponse(response, resource, &page); err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		continueToken = page.Metadata.Continue
		if continueToken == "" {
			return items, nil
		}
	}
}

func discoverNamespacedResources(ctx context.Context, target clusterTarget, client *http.Client, headers http.Header) (map[string][]discoveredResource, error) {
	var coreVersions apiVersionList
	if err := getApiJson(ctx, target, client, headers, "/api", &coreVersions); err != nil {
		return nil, fmt.Errorf("discover core API versions: %w", err)
	}
	var groups apiGroupList
	if err := getApiJson(ctx, target, client, headers, "/apis", &groups); err != nil {
		return nil, fmt.Errorf("discover API groups: %w", err)
	}

	versionSet := make(map[string]struct{})
	preferredVersions := make(map[string]string, len(groups.Groups))
	for _, version := range coreVersions.Versions {
		versionSet[version] = struct{}{}
	}
	for _, group := range groups.Groups {
		if group.PreferredVersion.GroupVersion != "" {
			preferredVersions[group.Name] = group.PreferredVersion.GroupVersion
		}
		for _, version := range group.Versions {
			versionSet[version.GroupVersion] = struct{}{}
		}
	}
	groupVersions := make([]string, 0, len(versionSet))
	for version := range versionSet {
		groupVersions = append(groupVersions, version)
	}
	sort.Strings(groupVersions)
	if len(groupVersions) == 0 {
		return nil, errors.New("API discovery returned no group versions")
	}

	lists := make([]apiResourceList, len(groupVersions))
	errs := make([]error, len(groupVersions))
	semaphore := make(chan struct{}, maxConcurrent)
	var wait sync.WaitGroup
	for i, groupVersion := range groupVersions {
		wait.Add(1)
		go func(i int, groupVersion string) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			errs[i] = getApiJson(ctx, target, client, headers, apiGroupVersionPath(groupVersion), &lists[i])
		}(i, groupVersion)
	}
	wait.Wait()

	type groupKind struct {
		group string
		kind  string
	}
	preferredResources := make(map[groupKind]discoveredResource)
	for i, list := range lists {
		if errs[i] != nil {
			return nil, fmt.Errorf("discover resources for %s: %w", groupVersions[i], errs[i])
		}
		groupVersion := list.GroupVersion
		if groupVersion == "" {
			groupVersion = groupVersions[i]
		}
		for _, resource := range list.Resources {
			if !resource.Namespaced || !slices.Contains(resource.Verbs, "list") || strings.Contains(resource.Name, "/") || resource.Kind == "" {
				continue
			}
			group := ""
			if strings.Contains(groupVersion, "/") {
				group, _, _ = strings.Cut(groupVersion, "/")
			}
			key := groupKind{group: group, kind: strings.ToLower(resource.Kind)}
			candidate := discoveredResource{
				Kind:         resource.Kind,
				GroupVersion: groupVersion,
				Name:         resource.Name,
			}
			current, exists := preferredResources[key]
			candidatePreferred := groupVersion == preferredVersions[group]
			currentPreferred := current.GroupVersion == preferredVersions[group]
			if !exists || (candidatePreferred && !currentPreferred) || (candidatePreferred == currentPreferred && groupVersion < current.GroupVersion) {
				preferredResources[key] = candidate
			}
		}
	}
	resources := make(map[string][]discoveredResource)
	for _, resource := range preferredResources {
		kind := strings.ToLower(resource.Kind)
		resources[kind] = append(resources[kind], resource)
	}
	for kind := range resources {
		sort.Slice(resources[kind], func(i, j int) bool {
			return resources[kind][i].GroupVersion < resources[kind][j].GroupVersion
		})
	}
	return resources, nil
}

func apiGroupVersionPath(groupVersion string) string {
	if strings.Contains(groupVersion, "/") {
		return "/apis/" + groupVersion
	}
	return "/api/" + groupVersion
}

func apiDoRequest(ctx context.Context, client *http.Client, headers http.Header, requestURL *url.URL, resource string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		for _, headerValue := range value {
			request.Header.Add(key, headerValue)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", resource, err)
	}
	return response, nil
}

func getApiJson(ctx context.Context, target clusterTarget, client *http.Client, headers http.Header, path string, result any) error {
	endpoint := strings.TrimRight(target.cluster.Server, "/") + path
	requestURL, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	response, err := apiDoRequest(ctx, client, headers, requestURL, path)
	if err != nil {
		return err
	}
	return decodeAPIResponse(response, path, result)
}

func decodeAPIResponse(response *http.Response, resource string, result any) error {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		return fmt.Errorf("%s: API returned %s: %s", resource, response.Status, strings.TrimSpace(string(message)))
	}
	decodeErr := json.NewDecoder(response.Body).Decode(result)
	closeErr := response.Body.Close()
	if decodeErr != nil {
		return fmt.Errorf("decode %s response: %w", resource, decodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s response: %w", resource, closeErr)
	}
	return nil
}

func makeHttpClient(ctx context.Context, target clusterTarget) (*http.Client, http.Header, error) {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: target.cluster.InsecureSkipTLSVerify,
		ServerName:         target.cluster.TLSServerName,
	}
	if target.cluster.CertificateAuthorityData != "" || target.cluster.CertificateAuthority != "" {
		caData, err := configBytes(target.cluster.CertificateAuthorityData, target.cluster.CertificateAuthority, target.baseDir)
		if err != nil {
			return nil, nil, fmt.Errorf("read certificate authority: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caData) {
			return nil, nil, errors.New("certificate authority contains no valid certificates")
		}
		tlsConfig.RootCAs = roots
	}

	user := target.user
	headers := make(http.Header)
	if user.Exec != nil {
		credential, err := runExecCredential(ctx, user.Exec)
		if err != nil {
			return nil, nil, err
		}
		user.Token = credential.Status.Token
		if credential.Status.ClientCertificateData != "" || credential.Status.ClientKeyData != "" {
			user.ClientCertificateData = base64.StdEncoding.EncodeToString([]byte(credential.Status.ClientCertificateData))
			user.ClientKeyData = base64.StdEncoding.EncodeToString([]byte(credential.Status.ClientKeyData))
		}
	}
	if user.TokenFile != "" {
		token, err := os.ReadFile(resolvePath(target.baseDir, user.TokenFile))
		if err != nil {
			return nil, nil, fmt.Errorf("read token file: %w", err)
		}
		user.Token = strings.TrimSpace(string(token))
	}
	if user.Token != "" {
		headers.Set("Authorization", "Bearer "+user.Token)
	} else if user.Username != "" {
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username+":"+user.Password)))
	}
	certData, err := configBytes(user.ClientCertificateData, user.ClientCertificate, target.baseDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read client certificate: %w", err)
	}
	keyData, err := configBytes(user.ClientKeyData, user.ClientKey, target.baseDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read client key: %w", err)
	}
	if len(certData) > 0 || len(keyData) > 0 {
		if len(certData) == 0 || len(keyData) == 0 {
			return nil, nil, errors.New("both client certificate and client key are required")
		}
		certificate, err := tls.X509KeyPair(certData, keyData)
		if err != nil {
			return nil, nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport, Timeout: requestTimeout}, headers, nil
}

type execCredential struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     struct {
		Token                 string `json:"token"`
		ClientCertificateData string `json:"clientCertificateData"`
		ClientKeyData         string `json:"clientKeyData"`
	} `json:"status"`
}

func runExecCredential(ctx context.Context, config *execConfig) (execCredential, error) {
	var credential execCredential
	if config.Command == "" {
		return credential, errors.New("exec credential command is empty")
	}
	commandCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, config.Command, config.Args...)
	cmd.Env = os.Environ()
	for _, entry := range config.Env {
		cmd.Env = append(cmd.Env, entry.Name+"="+entry.Value)
	}
	execInfo, _ := json.Marshal(map[string]any{
		"apiVersion": config.APIVersion,
		"kind":       "ExecCredential",
		"spec":       map[string]bool{"interactive": false},
	})
	cmd.Env = append(cmd.Env, "KUBERNETES_EXEC_INFO="+string(execInfo))
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return credential, fmt.Errorf("exec credential command failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return credential, fmt.Errorf("run exec credential command: %w", err)
	}
	if err := json.Unmarshal(output, &credential); err != nil {
		return credential, fmt.Errorf("parse exec credential output: %w", err)
	}
	if credential.APIVersion != config.APIVersion || credential.Kind != "ExecCredential" {
		return credential, fmt.Errorf("exec credential returned unexpected %s %s", credential.APIVersion, credential.Kind)
	}
	if credential.Status.Token == "" && (credential.Status.ClientCertificateData == "" || credential.Status.ClientKeyData == "") {
		return credential, errors.New("exec credential returned no token or client certificate")
	}
	return credential, nil
}

func configBytes(encoded, file, baseDir string) ([]byte, error) {
	if encoded != "" {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode embedded data: %w", err)
		}
		return data, nil
	}
	if file == "" {
		return nil, nil
	}
	return os.ReadFile(resolvePath(baseDir, file))
}

func resolvePath(baseDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

func buildPageData(results []clusterResult, selectedResource string, selectedClusters map[int]bool) pageData {
	set := make(map[string]struct{})
	resourceKinds := make(map[string]resourceKindOption)
	for _, result := range results {
		for _, namespace := range result.Namespaces {
			set[namespace] = struct{}{}
		}
		for key, resources := range result.Resources {
			label := ""
			for _, resource := range resources {
				if label == "" || resource.Kind < label {
					label = resource.Kind
				}
			}
			resourceKinds[key] = resourceKindOption{Key: key, Label: label}
		}
	}
	namespaces := make([]string, 0, len(set))
	for namespace := range set {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	if _, ok := resourceKinds[defaultResourceKey]; !ok {
		resourceKinds[defaultResourceKey] = resourceKindOption{Key: defaultResourceKey, Label: "Pod"}
	}
	kindOptions := make([]resourceKindOption, 0, len(resourceKinds))
	for _, option := range resourceKinds {
		kindOptions = append(kindOptions, option)
	}
	sort.Slice(kindOptions, func(i, j int) bool {
		if kindOptions[i].Label == kindOptions[j].Label {
			return kindOptions[i].Key < kindOptions[j].Key
		}
		return kindOptions[i].Label < kindOptions[j].Label
	})
	return pageData{
		Clusters:         results,
		Namespaces:       namespaces,
		ResourceKinds:    kindOptions,
		SelectedResource: selectedResource,
		SelectedClusters: selectedClusters,
	}
}

func selectedClusterIndexes(rawQuery string, clusterNames []string) (map[int]bool, error) {
	selected := make(map[int]bool, len(clusterNames))
	var selections [][]string
	for _, parameter := range strings.Split(rawQuery, "&") {
		if parameter == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(parameter, "=")
		key, err := url.QueryUnescape(rawKey)
		if err != nil || key != "clusters" {
			continue
		}
		values := strings.Split(rawValue, ",")
		for index, value := range values {
			values[index], err = url.QueryUnescape(value)
			if err != nil {
				return nil, fmt.Errorf("invalid cluster selection %q", value)
			}
		}
		selections = append(selections, values)
	}
	if len(selections) == 0 {
		for index := range clusterNames {
			selected[index] = true
		}
		return selected, nil
	}
	if len(selections) != 1 {
		return nil, errors.New("clusters parameter must be specified once")
	}
	if len(selections[0]) == 1 && selections[0][0] == "" {
		return selected, nil
	}
	clusterIndexes := make(map[string]int, len(clusterNames))
	for index, name := range clusterNames {
		clusterIndexes[name] = index
	}
	for _, name := range selections[0] {
		index, ok := clusterIndexes[name]
		if !ok {
			return nil, fmt.Errorf("invalid cluster selection %q", name)
		}
		selected[index] = true
	}
	return selected, nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
