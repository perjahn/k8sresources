package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestLoadTargetsUsesCurrentContextAndLoadsEveryCluster(t *testing.T) {
	config := `apiVersion: v1
current-context: chosen
clusters:
- name: first
  cluster:
    server: https://first.example
- name: second
  cluster:
    server: https://second.example
contexts:
- name: chosen
  context:
    cluster: first
    user: preferred
- name: alternate
  context:
    cluster: first
    user: alternate
- name: second-context
  context:
    cluster: second
    user: second-user
users:
- name: preferred
  user:
    token: preferred-token
- name: alternate
  user:
    token: alternate-token
- name: second-user
  user:
    token: second-token
`
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	targets, err := loadTargets(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d clusters, want 2", len(targets))
	}
	if targets[0].user.Token != "preferred-token" {
		t.Errorf("selected token = %q, want current-context token", targets[0].user.Token)
	}
	if targets[1].user.Token != "second-token" {
		t.Errorf("second cluster token = %q, want second-token", targets[1].user.Token)
	}
	if targets[0].baseDir != filepath.Dir(path) {
		t.Errorf("base directory = %q, want %q", targets[0].baseDir, filepath.Dir(path))
	}
}

func TestLoadTargetsExcludesMatchingClusterNames(t *testing.T) {
	config := `apiVersion: v1
clusters:
- name: production-cluster
  cluster:
    server: https://dev.example
- name: development
  cluster:
    server: https://prod.example
`
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	targets, err := loadTargets(path, regexp.MustCompile(`prod`))
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].name != "development" {
		t.Fatalf("targets = %#v, want only development", targets)
	}
}

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("K8SGETNS_TEST_VALUE", "from environment")
	if got := envOrDefault("K8SGETNS_TEST_VALUE", "fallback"); got != "from environment" {
		t.Errorf("envOrDefault() = %q, want environment value", got)
	}
	if got := envOrDefault("K8SGETNS_TEST_UNSET", "fallback"); got != "fallback" {
		t.Errorf("envOrDefault() = %q, want fallback", got)
	}
}

func TestListNamespacesUsesBearerTokenAndFollowsContinuation(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("authorization header = %q", got)
		}
		if r.URL.Path != "/api/v1/namespaces" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != listPageLimit {
			t.Errorf("limit = %q", r.URL.Query().Get("limit"))
		}
		if requests == 1 {
			if got := r.URL.Query().Get("continue"); got != "" {
				t.Errorf("first continue token = %q, want empty", got)
			}
			fmt.Fprint(w, `{"metadata":{"continue":"next page/+"},"items":[{"metadata":{"name":"zeta"}}]}`)
			return
		}
		if got := r.URL.Query().Get("continue"); got != "next page/+" {
			t.Errorf("second continue token = %q", got)
		}
		fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"name":"alpha"}}]}`)
	}))
	defer server.Close()

	target := clusterTarget{
		name:    "test",
		cluster: clusterInfo{Server: server.URL},
		user:    userInfo{Token: "test-token"},
	}
	client, headers, err := makeHttpClient(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	namespaces, err := listNamespaces(context.Background(), target, client, headers)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("got %d requests, want 2", requests)
	}
	if got := strings.Join(namespaces, ","); got != "alpha,zeta" {
		t.Errorf("namespaces = %q, want alpha,zeta", got)
	}
}

func TestListResourceCountsUsesGroupVersionAndFollowsContinuation(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/apis/apps/v1/deployments" {
			t.Errorf("path = %q, want /apis/apps/v1/deployments", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != listPageLimit {
			t.Errorf("limit = %q, want %q", r.URL.Query().Get("limit"), listPageLimit)
		}
		if requests == 1 {
			if got := r.URL.Query().Get("continue"); got != "" {
				t.Errorf("first continue token = %q, want empty", got)
			}
			fmt.Fprint(w, `{"metadata":{"continue":"next"},"items":[{"metadata":{"namespace":"default"}},{"metadata":{"namespace":"kube-system"}}]}`)
			return
		}
		if got := r.URL.Query().Get("continue"); got != "next" {
			t.Errorf("second continue token = %q, want next", got)
		}
		fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"namespace":"default"}}]}`)
	}))
	defer server.Close()

	target := clusterTarget{name: "test", cluster: clusterInfo{Server: server.URL}}
	client, headers, err := makeHttpClient(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	resource := []discoveredResource{{GroupVersion: "apps/v1", Name: "deployments", Kind: "Deployment"}}
	counts, err := listResourceCounts(context.Background(), target, client, headers, resource)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("got %d requests, want 2", requests)
	}
	if counts.Counts["default"] != 2 || counts.Counts["kube-system"] != 1 {
		t.Errorf("resource counts = %#v, want default: 2 and kube-system: 1", counts.Counts)
	}
}

func TestListResourceCountsAggregatesPodImagesByNamespace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pods" {
			t.Errorf("path = %q, want /api/v1/pods", r.URL.Path)
		}
		fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"namespace":"default"},"spec":{"containers":[{"image":"registry.example/app:v1"},{"image":"registry.example/app:v1"}],"initContainers":[{"image":"busybox:1"}]}},{"metadata":{"namespace":"default"},"spec":{"containers":[{"image":"registry.example/app:v1"},{"image":"sidecar:v2"}],"ephemeralContainers":[{"image":"sidecar:v2"}]}},{"metadata":{"namespace":"kube-system"},"spec":{"containers":[]}}]}`)
	}))
	defer server.Close()

	target := clusterTarget{name: "test", cluster: clusterInfo{Server: server.URL}}
	client, headers, err := makeHttpClient(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	result, err := listResourceCounts(context.Background(), target, client, headers, []discoveredResource{
		{GroupVersion: "v1", Name: "pods", Kind: "Pod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Counts["default"] != 2 || result.Counts["kube-system"] != 1 {
		t.Errorf("pod counts = %#v, want default: 2 and kube-system: 1", result.Counts)
	}
	wantImages := map[string]int{
		"registry.example/app:v1": 2,
		"busybox:1":               1,
		"sidecar:v2":              1,
	}
	if !reflect.DeepEqual(result.PodImageCounts["default"], wantImages) {
		t.Errorf("pod image counts = %#v, want %#v", result.PodImageCounts["default"], wantImages)
	}
}

func TestClusterTooltipListsSortedPodImageCounts(t *testing.T) {
	cluster := clusterResult{
		Name: "dev",
		PodImageCounts: map[string]map[string]int{
			"default": {
				"registry.example/z:v1": 1,
				"registry.example/a:v2": 2,
			},
		},
	}
	want := "dev\nregistry.example/a:v2 (2)\nregistry.example/z:v1"
	if got := clusterTooltip(cluster, "default", defaultResourceKey); got != want {
		t.Errorf("clusterTooltip() = %q, want %q", got, want)
	}
	if got := clusterTooltip(cluster, "default", "deployment"); got != "dev" {
		t.Errorf("non-pod clusterTooltip() = %q, want cluster name only", got)
	}
}

func TestPageTemplateRendersPodImageTooltip(t *testing.T) {
	data := pageData{
		Clusters: []clusterResult{{
			Name:           "dev",
			Namespaces:     []string{"default"},
			ResourceCounts: map[string]int{"default": 2},
			PodImageCounts: map[string]map[string]int{
				"default": {"registry.example/app:v1": 2},
			},
		}},
		Namespaces:       []string{"default"},
		ResourceKinds:    []resourceKindOption{{Key: defaultResourceKey, Label: "Pod"}},
		SelectedResource: defaultResourceKey,
		SelectedClusters: map[int]bool{0: true},
	}
	pageTemplate, err := loadPageTemplate()
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := pageTemplate.Execute(&output, data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "title=\"dev\nregistry.example/app:v1 (2)\"") {
		t.Errorf("rendered pod cell does not contain its multiline image tooltip: %s", output.String())
	}
}

func TestDiscoverNamespacedResourcesAcrossVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			fmt.Fprint(w, `{"versions":["v1"]}`)
		case "/apis":
			fmt.Fprint(w, `{"groups":[{"name":"apps","preferredVersion":{"groupVersion":"apps/v2"},"versions":[{"groupVersion":"apps/v1"},{"groupVersion":"apps/v2"}]},{"name":"batch","preferredVersion":{"groupVersion":"batch/v1"},"versions":[{"groupVersion":"batch/v1"}]}]}`)
		case "/api/v1/namespaces":
			fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"name":"default"}}]}`)
		case "/api/v1":
			fmt.Fprint(w, `{"groupVersion":"v1","resources":[{"name":"pods","kind":"Pod","namespaced":true,"verbs":["get","list"]},{"name":"nodes","kind":"Node","namespaced":false,"verbs":["list"]},{"name":"pods/status","kind":"Pod","namespaced":true,"verbs":["list"]}]}`)
		case "/api/v1/pods":
			fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"namespace":"default"}}]}`)
		case "/apis/apps/v1":
			fmt.Fprint(w, `{"groupVersion":"apps/v1","resources":[{"name":"deployments","kind":"Deployment","namespaced":true,"verbs":["list"]}]}`)
		case "/apis/apps/v2":
			fmt.Fprint(w, `{"groupVersion":"apps/v2","resources":[{"name":"deployments","kind":"Deployment","namespaced":true,"verbs":["list"]},{"name":"widgets","kind":"Widget","namespaced":true,"verbs":["get"]}]}`)
		case "/apis/batch/v1":
			fmt.Fprint(w, `{"groupVersion":"batch/v1","resources":[{"name":"deployments","kind":"Deployment","namespaced":true,"verbs":["list"]}]}`)
		case "/apis/apps/v2/deployments":
			fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"namespace":"default"}},{"metadata":{"namespace":"default"}}]}`)
		case "/apis/batch/v1/deployments":
			fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"namespace":"default"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := clusterTarget{name: "test", cluster: clusterInfo{Server: server.URL}}
	client, headers, err := makeHttpClient(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	resources, err := discoverNamespacedResources(context.Background(), target, client, headers)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pod", "deployment"} {
		if _, ok := resources[key]; !ok {
			t.Errorf("discovered resources missing %q", key)
		}
	}
	if got := len(resources["deployment"]); got != 2 {
		t.Errorf("same-kind resources = %d, want one preferred version from each API group", got)
	}
	if !slices.ContainsFunc(resources["deployment"], func(resource discoveredResource) bool {
		return resource.GroupVersion == "apps/v2"
	}) {
		t.Error("apps preferred version was not selected")
	}
	for _, key := range []string{"node", "pod/status", "widget"} {
		if _, ok := resources[key]; ok {
			t.Errorf("unexpectedly discovered non-listable or non-namespaced resource %q", key)
		}
	}

	results := queryClusters(context.Background(), []clusterTarget{target}, "deployment")
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("queryClusters() = %#v, want one successful cluster result", results)
	}
	if got := results[0].ResourceCounts["default"]; got != 3 {
		t.Errorf("deployment count = %d, want merged count 3", got)
	}
	podResults := queryClusters(context.Background(), []clusterTarget{target}, defaultResourceKey)
	if len(podResults) != 1 || podResults[0].Err != nil {
		t.Fatalf("default pod query = %#v, want one successful cluster result", podResults)
	}
	if got := podResults[0].ResourceCounts["default"]; got != 1 {
		t.Errorf("default pod count = %d, want 1", got)
	}
}

func TestPageTemplateRendersGridAndEscapesClusterNames(t *testing.T) {
	data := pageData{
		Clusters: []clusterResult{
			{Name: `<cluster&>`, Namespaces: []string{"default"}, ResourceCounts: map[string]int{"default": 3}},
			{Name: "offline", Err: fmt.Errorf("connection refused")},
		},
		Namespaces:       []string{"default"},
		ResourceKinds:    []resourceKindOption{{Key: defaultResourceKey, Label: "Pod"}},
		SelectedResource: defaultResourceKey,
		SelectedClusters: map[int]bool{0: true},
		HideEmpty:        true,
	}
	pageTemplate, err := loadPageTemplate()
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := pageTemplate.Execute(&output, data); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, expected := range []string{
		`<input id="hide-empty" name="hide_empty" value="1" type="checkbox" checked> hide empty`,
		`class="include-in-diff" checked data-cluster-index="0" data-cluster-name="&lt;cluster&amp;&gt;"`,
		`class="include-in-diff" data-cluster-index="1"`,
		`data-cluster-index="0" data-state="present" title="&lt;cluster&amp;&gt;"`,
		`data-cluster-index="1" data-state="unavailable" title="offline"`,
		`title="&lt;cluster&amp;&gt;"`, `title="offline"`, "default", ">3</td>", "Unavailable", "connection refused",
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("rendered HTML does not contain %q", expected)
		}
	}
	if !strings.Contains(html, `<link rel="stylesheet" href="/styles.css">`) {
		t.Error("rendered HTML does not link to the stylesheet")
	}
	if !strings.Contains(html, `<script src="/app.js" defer></script>`) {
		t.Error("rendered HTML does not load the client-side script")
	}
	if !strings.Contains(html, `<option value="pod" selected>Pod</option>`) {
		t.Error("rendered HTML does not select the chosen resource kind")
	}
	if strings.Contains(html, `<cluster&>`) {
		t.Error("cluster name was not HTML-escaped")
	}
}

func TestHandlerRetainsHideEmptySelection(t *testing.T) {
	response := httptest.NewRecorder()
	newHandler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?kind=pod&hide_empty=1", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), `<input id="hide-empty" name="hide_empty" value="1" type="checkbox" checked>`) {
		t.Error("hide-empty checkbox was not checked from the query parameter")
	}
}

func TestPageTemplateHighlightsRowsWithMissingNamespaces(t *testing.T) {
	data := pageData{
		Clusters: []clusterResult{
			{Name: "complete", Namespaces: []string{"shared", "required"}, ResourceCounts: map[string]int{"shared": 2, "required": 0}},
			{Name: "missing-required", Namespaces: []string{"shared"}, ResourceCounts: map[string]int{"shared": 1}},
			{Name: "offline", Namespaces: []string{"unknown"}, Err: fmt.Errorf("connection refused")},
		},
		Namespaces:       []string{"shared", "required", "unknown"},
		ResourceKinds:    []resourceKindOption{{Key: defaultResourceKey, Label: "Pod"}},
		SelectedResource: defaultResourceKey,
		SelectedClusters: map[int]bool{0: true, 1: true, 2: true},
	}
	pageTemplate, err := loadPageTemplate()
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := pageTemplate.Execute(&output, data); err != nil {
		t.Fatal(err)
	}

	html := output.String()
	if !strings.Contains(html, `<tr data-diff-row><th>shared</th>`) {
		t.Error("row present in every reachable cluster was highlighted")
	}
	if !strings.Contains(html, `<tr data-diff-row class="different"><th>required</th>`) {
		t.Error("row with differing namespace presence was not highlighted")
	}
	if !strings.Contains(html, `<tr data-diff-row><th>unknown</th>`) {
		t.Error("namespace only reported by an unavailable cluster was highlighted")
	}
	if !strings.Contains(html, `title="complete">0</td>`) {
		t.Error("namespace with no resources did not display zero")
	}
	if !strings.Contains(html, `title="missing-required"><span class="missing" aria-label="absent">&mdash;</span></td>`) {
		t.Error("missing namespace did not display a dash")
	}
}

func TestBuildPageDataUnionsResourceKinds(t *testing.T) {
	data := buildPageData([]clusterResult{
		{Resources: map[string][]discoveredResource{
			"pod":        {{Kind: "Pod", GroupVersion: "v1"}},
			"deployment": {{Kind: "Deployment", GroupVersion: "apps/v1"}},
		}},
		{Resources: map[string][]discoveredResource{
			"cronjob": {{Kind: "CronJob", GroupVersion: "batch/v1"}},
		}},
	}, "deployment", map[int]bool{0: true, 1: true})

	if data.SelectedResource != "deployment" {
		t.Errorf("selected resource = %q", data.SelectedResource)
	}
	for _, key := range []string{"pod", "deployment", "cronjob"} {
		if !slices.ContainsFunc(data.ResourceKinds, func(option resourceKindOption) bool { return option.Key == key }) {
			t.Errorf("resource options do not contain %q", key)
		}
	}
}

func TestSelectedClusterIndexes(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		want      map[int]bool
		wantError bool
	}{
		{name: "default all", want: map[int]bool{0: true, 1: true, 2: true}},
		{name: "selected clusters", query: "clusters=alpha,prod%2Ceast", want: map[int]bool{0: true, 2: true}},
		{name: "none selected", query: "clusters=", want: map[int]bool{}},
		{name: "invalid cluster", query: "clusters=missing", wantError: true},
		{name: "repeated parameter", query: "clusters=alpha&clusters=beta", wantError: true},
	}
	clusterNames := []string{"alpha", "beta", "prod,east"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectedClusterIndexes(test.query, clusterNames)
			if (err != nil) != test.wantError {
				t.Fatalf("selectedClusterIndexes() error = %v, wantError %v", err, test.wantError)
			}
			if err == nil && !reflect.DeepEqual(got, test.want) {
				t.Errorf("selectedClusterIndexes() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestQueryClustersKeepsDiscoveredKindsWhenNamespaceListingFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces":
			http.Error(w, "forbidden", http.StatusForbidden)
		case "/api":
			fmt.Fprint(w, `{"versions":["v1"]}`)
		case "/apis":
			fmt.Fprint(w, `{"groups":[]}`)
		case "/api/v1":
			fmt.Fprint(w, `{"groupVersion":"v1","resources":[{"name":"pods","kind":"Pod","namespaced":true,"verbs":["list"]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := clusterTarget{name: "restricted", cluster: clusterInfo{Server: server.URL}}
	results := queryClusters(context.Background(), []clusterTarget{target}, defaultResourceKey)
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("queryClusters() = %#v, want one unavailable cluster", results)
	}
	if _, ok := results[0].Resources[defaultResourceKey]; !ok {
		t.Fatal("resource discovery was lost when namespace listing failed")
	}
}

func TestDiskWebAssetsOverrideEmbeddedAssets(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("index.html", []byte("<html>disk page</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("styles.css", []byte("body { color: red; }"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("app.js", []byte("console.log('disk script');"), 0600); err != nil {
		t.Fatal(err)
	}

	handler := newHandler(nil)
	for _, test := range []struct {
		path     string
		expected string
	}{
		{path: "/", expected: "disk page"},
		{path: "/styles.css", expected: "color: red"},
		{path: "/app.js", expected: "disk script"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != http.StatusOK {
			t.Errorf("%s status = %d, want %d", test.path, response.Code, http.StatusOK)
		}
		if !strings.Contains(response.Body.String(), test.expected) {
			t.Errorf("%s response does not contain disk override %q", test.path, test.expected)
		}
	}
}

func TestEmbeddedWebAssetsAreServed(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		path        string
		contentType string
		expected    []string
	}{
		{path: "/styles.css", contentType: "text/css", expected: []string{".different { background-color: #fff8cc; }"}},
		{path: "/app.js", contentType: "text/javascript", expected: []string{
			`states.includes("present") && states.includes("missing")`,
			`const selectedIndices = new Set(`,
			`cell.dataset.state === "missing"`,
			`cell.textContent.trim() === "0"`,
			`const useAllClusters = selectedIndices.size === 0;`,
			`.filter((cell) => useAllClusters || selectedIndices.has(cell.dataset.clusterIndex))`,
			`row.hidden = hideEmptyCheckbox.checked && isEmpty;`,
			`resourceKindSelect.focus();`,
			`selectedNames.map(encodeURIComponent).join(",")`,
			`event.preventDefault();`,
			`url.searchParams.set("hide_empty", "1")`,
			`url.searchParams.delete("clusters")`,
			`selectedNames.length === checkboxes.length`,
			`url.searchParams.get("kind") === "pod"`,
			`url.searchParams.get("hide_empty") !== "1"`,
			`params.delete("kind")`,
			"url.search = [query, clusters].filter(Boolean).join(\"&\");",
			`window.location.assign(url.href)`,
		}},
	} {
		response := httptest.NewRecorder()
		newHandler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != http.StatusOK {
			t.Errorf("%s status = %d, want %d", test.path, response.Code, http.StatusOK)
		}
		if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, test.contentType) {
			t.Errorf("%s content type = %q, want %s", test.path, got, test.contentType)
		}
		for _, expected := range test.expected {
			if !strings.Contains(response.Body.String(), expected) {
				t.Errorf("%s response does not contain %q", test.path, expected)
			}
		}
	}
}
