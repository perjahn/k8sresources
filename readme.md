# k8sresources

A small go http service that reads cluster entries from a kubeconfig and displays counts of namespaced resource kinds for each namespace in each cluster as an html matrix.

## Run

```sh
KUBECONFIG=~/.kube/config ADDR=127.0.0.1:8080 EXCLUDE_CLUSTERS='prod|staging' go run .
```

Open <http://127.0.0.1:8080/>. Configuration can be set with `KUBECONFIG` (defaults to `~/.kube/config`), `ADDR` (defaults to `127.0.0.1:8080`), and `EXCLUDE_CLUSTERS` (an optional regular expression matched against kubeconfig cluster names; matching clusters are omitted). The corresponding `-kubeconfig`, `-addr`, and `-exclude-clusters` flags are also supported and override their environment variable defaults.

The service discovers listable namespaced resources, including custom resources, across all clusters. Select a kind above the table to display its per-namespace count; the default is pods. Pod count tooltips list the cluster name followed by each unique container image url; images used by multiple pods include their pod count in parentheses. The “hide empty” checkbox hides rows where every selected cluster either lacks the namespace or has zero resources of the selected kind; with no clusters selected, it evaluates all clusters. Its state is retained when changing kind. Non-default kind url values are lowercase names such as `kind=deployment`; the default pod kind is omitted. Same-named kinds from multiple api groups are combined, using each group's preferred version. It follows paginated api responses and refreshes data for each page request. A dash indicates that a namespace is absent from a cluster. Cluster selections for difference highlighting use a single comma-separated `clusters` url parameter containing cluster names, such as `clusters=dev,staging`, and persist across reloads; the parameter is omitted when all clusters are selected and an empty value means none are selected. Commas within a cluster name are percent-encoded. The current context's credentials are used for its cluster. For other clusters, the lexically first context referencing that cluster is used. Clusters without a matching context are queried without user credentials. Supported kubeconfig authentication includes bearer tokens, token files, basic credentials, client certificates, and `exec` credential plugins. Cluster certificate authorities and client certificate/key files are resolved relative to the kubeconfig file when specified by path.

To customize the page, styles, or client-side behavior, put `index.html`, `styles.css`, or `app.js` in the process's working directory. Disk files override the embedded defaults and are checked on each request, so edits take effect without rebuilding or restarting.

The listener defaults to loopback only. A cluster that cannot be queried is shown as unavailable, with its error in the column heading.

## Test

```sh
go test ./...
```
