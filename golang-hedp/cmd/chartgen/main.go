// Command chartgen builds a synthetic Helm chart library plus a release
// blueprint, sized to approximate a real platform's static assets.
//
// The point is not to produce useful charts. It is to produce a workload with
// the same *shape* as a real one so the render benchmarks mean something:
//
//   - templates/    - parsed and executed by the Helm engine on every render.
//   - files/        - shipped in the chart, read via .Files, never parsed.
//   - crds/         - shipped in the chart, never read and never parsed.
//
// Those three buckets have wildly different costs, so "55 MB of charts" is a
// meaningless number until you know how it splits. chartgen lets you dial the
// split with -template-frac and -crd-frac and watch the benchmarks move.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "chartgen:", err)
		os.Exit(1)
	}
}

type options struct {
	outDir       string
	totalBytes   int64
	charts       int
	templateFrac float64
	crdFrac      float64
	seed         int64
}

func run() error {
	var o options
	flag.StringVar(&o.outDir, "out", "testdata/library", "output directory for charts + blueprint")
	flag.Int64Var(&o.totalBytes, "bytes", 55<<20, "approximate total size of the generated chart library")
	flag.IntVar(&o.charts, "charts", 40, "number of charts to generate")
	flag.Float64Var(&o.templateFrac, "template-frac", 0.35, "fraction of bytes that live in templates/ (parsed by the engine)")
	flag.Float64Var(&o.crdFrac, "crd-frac", 0.40, "fraction of bytes that live in crds/ (never parsed, never read)")
	flag.Int64Var(&o.seed, "seed", 1, "PRNG seed, so output is reproducible")
	flag.Parse()

	if o.charts < 1 {
		return fmt.Errorf("-charts must be >= 1")
	}
	if o.templateFrac+o.crdFrac > 1 {
		return fmt.Errorf("-template-frac + -crd-frac must be <= 1 (remainder goes to files/)")
	}

	if err := os.RemoveAll(o.outDir); err != nil {
		return err
	}
	chartsDir := filepath.Join(o.outDir, "charts")
	if err := os.MkdirAll(chartsDir, 0o755); err != nil {
		return err
	}

	rng := rand.New(rand.NewSource(o.seed))
	specs := planCharts(o, rng)

	var stats libraryStats
	for _, spec := range specs {
		s, err := writeChart(filepath.Join(chartsDir, spec.Name), spec, rng)
		if err != nil {
			return fmt.Errorf("chart %s: %w", spec.Name, err)
		}
		stats.add(s)
	}

	bp := buildBlueprint(specs)
	if err := os.WriteFile(filepath.Join(o.outDir, "blueprint.yaml"), []byte(bp), 0o644); err != nil {
		return err
	}

	fmt.Printf("wrote %d charts to %s\n", len(specs), chartsDir)
	fmt.Printf("  templates/ %6.1f MB in %4d files  (parsed + executed every render)\n", mb(stats.templateBytes), stats.templateFiles)
	fmt.Printf("  files/     %6.1f MB in %4d files  (read via .Files, not parsed)\n", mb(stats.fileBytes), stats.fileFiles)
	fmt.Printf("  crds/      %6.1f MB in %4d files  (never touched by the engine)\n", mb(stats.crdBytes), stats.crdFiles)
	fmt.Printf("  total      %6.1f MB in %4d files\n", mb(stats.total()), stats.totalFiles())
	return nil
}

func mb(b int64) float64 { return float64(b) / (1 << 20) }

type libraryStats struct {
	templateBytes, fileBytes, crdBytes int64
	templateFiles, fileFiles, crdFiles int
}

func (s *libraryStats) add(o libraryStats) {
	s.templateBytes += o.templateBytes
	s.fileBytes += o.fileBytes
	s.crdBytes += o.crdBytes
	s.templateFiles += o.templateFiles
	s.fileFiles += o.fileFiles
	s.crdFiles += o.crdFiles
}

func (s libraryStats) total() int64    { return s.templateBytes + s.fileBytes + s.crdBytes }
func (s libraryStats) totalFiles() int { return s.templateFiles + s.fileFiles + s.crdFiles }

// chartSpec describes one chart's byte budget and its place in the blueprint.
type chartSpec struct {
	Name  string
	Layer string // base | platform | app
	Index int

	templateBytes int64
	fileBytes     int64
	crdBytes      int64
}

// planCharts spreads the byte budget over the charts. Real libraries are
// lopsided - a couple of enormous charts (the ones vendoring an operator's
// CRDs) and a long tail of small ones - so the distribution is skewed rather
// than uniform. That matters because it means p99 render latency is driven by
// which charts a customer happens to select, not by the average chart size.
func planCharts(o options, rng *rand.Rand) []chartSpec {
	layers := []string{"base", "platform", "app"}

	weights := make([]float64, o.charts)
	var totalWeight float64
	for i := range weights {
		// Long tail: most charts near 1, a few 10-25x bigger.
		w := 1.0
		switch {
		case i%13 == 0:
			w = 15 + rng.Float64()*10
		case i%5 == 0:
			w = 4 + rng.Float64()*3
		default:
			w = 0.6 + rng.Float64()
		}
		weights[i] = w
		totalWeight += w
	}

	specs := make([]chartSpec, o.charts)
	filesFrac := 1 - o.templateFrac - o.crdFrac
	for i := range specs {
		share := float64(o.totalBytes) * weights[i] / totalWeight
		specs[i] = chartSpec{
			Name:          fmt.Sprintf("%s-%02d", layers[i%len(layers)], i),
			Layer:         layers[i%len(layers)],
			Index:         i,
			templateBytes: int64(share * o.templateFrac),
			fileBytes:     int64(share * filesFrac),
			crdBytes:      int64(share * o.crdFrac),
		}
	}
	return specs
}

func writeChart(dir string, spec chartSpec, rng *rand.Rand) (libraryStats, error) {
	var stats libraryStats

	write := func(rel string, content string, bucket *int64, count *int) error {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
		*bucket += int64(len(content))
		*count++
		return nil
	}

	// Chart.yaml and values.yaml are metadata, not part of any byte bucket -
	// they are tiny and loaded once.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return stats, err
	}
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML(spec)), 0o644); err != nil {
		return stats, err
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(valuesYAML(spec)), 0o644); err != nil {
		return stats, err
	}

	// The fixed set of "real" templates every chart gets. These are the ones
	// that actually exercise the engine's function map: include, toYaml,
	// required, sha256sum, range, nested conditionals.
	fixed := map[string]string{
		"templates/_helpers.tpl":    helpersTpl,
		"templates/deployment.yaml": deploymentTpl,
		"templates/service.yaml":    serviceTpl,
		"templates/configmap.yaml":  configmapTpl,
		"templates/ingress.yaml":    ingressTpl,
		"templates/hpa.yaml":        hpaTpl,
		"templates/rbac.yaml":       rbacTpl,
		"templates/assets.yaml":     assetsTpl,
		"templates/NOTES.txt":       notesTpl,
	}
	names := make([]string, 0, len(fixed))
	for n := range fixed {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := write(n, fixed[n], &stats.templateBytes, &stats.templateFiles); err != nil {
			return stats, err
		}
	}

	// Bulk templates: dashboard ConfigMaps with a large embedded payload and
	// enough template actions that the engine cannot shortcut them. This is
	// the realistic shape of a multi-MB chart's templates/ directory.
	remaining := spec.templateBytes - stats.templateBytes
	for i := 0; remaining > 0; i++ {
		size := min64(remaining, 96<<10)
		body := dashboardTemplate(spec.Name, i, size, rng)
		if err := write(fmt.Sprintf("templates/dashboards/dashboard-%03d.yaml", i), body, &stats.templateBytes, &stats.templateFiles); err != nil {
			return stats, err
		}
		remaining -= int64(len(body))
	}

	// files/: shipped with the chart, reachable through .Files.Glob, never
	// parsed as a template.
	for i, remaining := 0, spec.fileBytes; remaining > 0; i++ {
		size := min64(remaining, 128<<10)
		body := jsonBlob(size, rng)
		if err := write(fmt.Sprintf("files/assets/asset-%03d.json", i), body, &stats.fileBytes, &stats.fileFiles); err != nil {
			return stats, err
		}
		remaining -= int64(len(body))
	}

	// crds/: pure ballast as far as rendering is concerned. Helm applies these
	// verbatim; the engine never opens them.
	for i, remaining := 0, spec.crdBytes; remaining > 0; i++ {
		size := min64(remaining, 256<<10)
		body := crdYAML(spec.Name, i, size, rng)
		if err := write(fmt.Sprintf("crds/crd-%03d.yaml", i), body, &stats.crdBytes, &stats.crdFiles); err != nil {
			return stats, err
		}
		remaining -= int64(len(body))
	}

	return stats, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func chartYAML(spec chartSpec) string {
	return fmt.Sprintf(`apiVersion: v2
name: %s
description: Synthetic chart (%s layer) for HEDP render benchmarking
type: application
version: 1.0.0
appVersion: "1.0.0"
`, spec.Name, spec.Layer)
}

func valuesYAML(spec chartSpec) string {
	return fmt.Sprintf(`replicaCount: 1

image:
  repository: registry.example.com/%s
  tag: "1.0.0"
  pullPolicy: IfNotPresent

service:
  type: ClusterIP
  port: 8080

ingress:
  enabled: false
  className: nginx
  hosts: []

autoscaling:
  enabled: false
  minReplicas: 1
  maxReplicas: 10
  targetCPUUtilizationPercentage: 80

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: "1"
    memory: 512Mi

serviceAccount:
  create: true
  name: ""

env: []
extraLabels: {}
nodeSelector: {}
tolerations: []
affinity: {}

# Populated by the HEDP renderer from upstream releases' exports.
deps: {}

# Populated by the HEDP renderer from the customer configuration.
customer:
  id: ""
  tier: ""
  region: ""
  features: []

assets:
  enabled: false
`, spec.Name)
}

const helpersTpl = `{{- define "chart.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "chart.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "chart.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "chart.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "chart.labels" -}}
helm.sh/chart: {{ include "chart.chart" . }}
app.kubernetes.io/name: {{ include "chart.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
hedp.example.com/customer: {{ .Values.customer.id | default "unknown" | quote }}
hedp.example.com/tier: {{ .Values.customer.tier | default "none" | quote }}
{{- with .Values.extraLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "chart.selectorLabels" -}}
app.kubernetes.io/name: {{ include "chart.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "chart.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "chart.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
`

const deploymentTpl = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "chart.fullname" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
spec:
  {{- if not .Values.autoscaling.enabled }}
  replicas: {{ .Values.replicaCount }}
  {{- end }}
  selector:
    matchLabels:
      {{- include "chart.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      annotations:
        checksum/config: {{ toYaml .Values.customer | sha256sum }}
      labels:
        {{- include "chart.selectorLabels" . | nindent 8 }}
    spec:
      serviceAccountName: {{ include "chart.serviceAccountName" . }}
      containers:
        - name: {{ .Chart.Name }}
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          ports:
            - name: http
              containerPort: {{ .Values.service.port }}
              protocol: TCP
          env:
            - name: CUSTOMER_ID
              value: {{ .Values.customer.id | quote }}
            - name: REGION
              value: {{ .Values.customer.region | quote }}
            {{- range $name, $dep := .Values.deps }}
            - name: {{ $name | upper | replace "-" "_" }}_ENDPOINT
              value: {{ $dep.endpoint | quote }}
            {{- end }}
            {{- range .Values.env }}
            - name: {{ .name }}
              value: {{ .value | quote }}
            {{- end }}
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
      {{- with .Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
`

const serviceTpl = `apiVersion: v1
kind: Service
metadata:
  name: {{ include "chart.fullname" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
spec:
  type: {{ .Values.service.type }}
  ports:
    - port: {{ .Values.service.port }}
      targetPort: http
      protocol: TCP
      name: http
  selector:
    {{- include "chart.selectorLabels" . | nindent 4 }}
`

const configmapTpl = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "chart.fullname" . }}-config
  labels:
    {{- include "chart.labels" . | nindent 4 }}
data:
  customer.yaml: |
    {{- toYaml .Values.customer | nindent 4 }}
  upstreams.yaml: |
    {{- if .Values.deps }}
    {{- toYaml .Values.deps | nindent 4 }}
    {{- else }}
    {}
    {{- end }}
`

const ingressTpl = `{{- if .Values.ingress.enabled }}
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "chart.fullname" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
spec:
  ingressClassName: {{ .Values.ingress.className }}
  rules:
    {{- range .Values.ingress.hosts }}
    - host: {{ .host | quote }}
      http:
        paths:
          - path: {{ .path | default "/" }}
            pathType: Prefix
            backend:
              service:
                name: {{ include "chart.fullname" $ }}
                port:
                  number: {{ $.Values.service.port }}
    {{- end }}
{{- end }}
`

const hpaTpl = `{{- if .Values.autoscaling.enabled }}
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ include "chart.fullname" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: {{ include "chart.fullname" . }}
  minReplicas: {{ .Values.autoscaling.minReplicas }}
  maxReplicas: {{ .Values.autoscaling.maxReplicas }}
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: {{ .Values.autoscaling.targetCPUUtilizationPercentage }}
{{- end }}
`

const rbacTpl = `{{- if .Values.serviceAccount.create }}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "chart.serviceAccountName" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "chart.fullname" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
rules:
  - apiGroups: [""]
    resources: ["configmaps", "secrets"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "chart.fullname" . }}
  labels:
    {{- include "chart.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ include "chart.fullname" . }}
subjects:
  - kind: ServiceAccount
    name: {{ include "chart.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
{{- end }}
`

// assetsTpl pulls the whole files/ tree into ConfigMaps. It is gated off by
// default: the point is to be able to switch on ".Files-heavy" rendering and
// see what reading (but not parsing) megabytes costs.
const assetsTpl = `{{- if .Values.assets.enabled }}
{{- range $path, $bytes := .Files.Glob "files/assets/*.json" }}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "chart.fullname" $ }}-{{ base $path | trimSuffix ".json" }}
  labels:
    {{- include "chart.labels" $ | nindent 4 }}
data:
  {{ base $path }}: |
    {{- $.Files.Get $path | nindent 4 }}
{{- end }}
{{- end }}
`

const notesTpl = `{{ include "chart.fullname" . }} deployed for customer {{ .Values.customer.id }} in {{ .Values.customer.region }}.
`

// dashboardTemplate produces a large ConfigMap template. The embedded payload
// is padded to `size`, and template actions are sprinkled through it so the
// engine has to walk the whole thing rather than treating it as one text node.
func dashboardTemplate(chartName string, idx int, size int64, rng *rand.Rand) string {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "chart.fullname" . }}-dash-%03d
  labels:
    {{- include "chart.labels" . | nindent 4 }}
  annotations:
    hedp.example.com/chart: %q
data:
  dashboard-%03d.json: |
    {
      "title": "{{ .Values.customer.id }} / %s / panel %d",
      "tags": [{{ range $i, $f := .Values.customer.features }}{{ if $i }}, {{ end }}{{ $f | quote }}{{ end }}],
      "panels": [
`, idx, chartName, idx, chartName, idx)

	panel := 0
	for int64(b.Len()) < size {
		fmt.Fprintf(&b, `        {
          "id": %d,
          "type": %q,
          "title": "{{ .Values.customer.tier }} panel %d",
          "datasource": {"uid": "{{ .Release.Name }}-ds"},
          "targets": [{"expr": "sum(rate(http_requests_total{job=\"%s\",instance=\"%s\"}[5m]))", "refId": "A"}],
          "fieldConfig": {"defaults": {"unit": "reqps", "custom": {"lineWidth": %d, "fillOpacity": %d}}},
          "gridPos": {"h": 8, "w": 12, "x": %d, "y": %d}
        },
`, panel, panelTypes[panel%len(panelTypes)], panel, chartName, randToken(rng, 24), 1+panel%3, panel%40, (panel%2)*12, (panel/2)*8)
		panel++
	}

	b.WriteString(`        {"id": 9999, "type": "row", "title": "end"}
      ]
    }
`)
	return b.String()
}

var panelTypes = []string{"timeseries", "stat", "gauge", "table", "heatmap", "barchart"}

func jsonBlob(size int64, rng *rand.Rand) string {
	var b strings.Builder
	b.WriteString("{\n  \"records\": [\n")
	for i := 0; int64(b.Len()) < size; i++ {
		fmt.Fprintf(&b, "    {\"id\": %d, \"key\": %q, \"value\": %q, \"weight\": %.4f},\n",
			i, randToken(rng, 16), randToken(rng, 48), rng.Float64())
	}
	b.WriteString("    {\"id\": -1, \"key\": \"eof\", \"value\": \"eof\", \"weight\": 0}\n  ]\n}\n")
	return b.String()
}

func crdYAML(chartName string, idx int, size int64, rng *rand.Rand) string {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widget%03d.%s.example.com
spec:
  group: %s.example.com
  scope: Namespaced
  names:
    plural: widget%03ds
    singular: widget%03d
    kind: Widget%03d
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
`, idx, chartName, chartName, idx, idx, idx)

	for i := 0; int64(b.Len()) < size; i++ {
		fmt.Fprintf(&b, `                field%05d:
                  type: string
                  description: %q
                  maxLength: %d
`, i, randToken(rng, 64), 64+i%256)
	}
	return b.String()
}

const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func randToken(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(b)
}

// buildBlueprint emits the release graph. The shape mirrors a real platform:
//
//	base     - always rendered, no dependencies, everything hangs off it.
//	platform - feature-gated, depends on base.
//	app      - feature-gated, depends on platform (and transitively on base).
//
// Dependencies are between *releases*, not charts, and carry data: each
// release exports a few values that its dependents receive under
// .Values.deps.<release>.
func buildBlueprint(specs []chartSpec) string {
	var b strings.Builder
	b.WriteString("# Generated by chartgen. The release graph the HEDP service renders.\n")
	b.WriteString("releases:\n")

	byLayer := map[string][]chartSpec{}
	for _, s := range specs {
		byLayer[s.Layer] = append(byLayer[s.Layer], s)
	}

	features := []string{"observability", "mesh", "search", "analytics", "cdn", "ml", "billing", "audit"}

	for _, s := range specs {
		var dependsOn []string
		var requires []string

		switch s.Layer {
		case "base":
			// Always on, no deps.
		case "platform":
			if base := byLayer["base"]; len(base) > 0 {
				dependsOn = append(dependsOn, base[s.Index%len(base)].Name)
			}
			requires = append(requires, features[s.Index%len(features)])
		case "app":
			if plat := byLayer["platform"]; len(plat) > 0 {
				dependsOn = append(dependsOn, plat[s.Index%len(plat)].Name)
			}
			if plat := byLayer["platform"]; len(plat) > 1 && s.Index%3 == 0 {
				// A second edge, so the graph is a DAG rather than a forest.
				dependsOn = append(dependsOn, plat[(s.Index+1)%len(plat)].Name)
			}
			requires = append(requires, features[(s.Index+3)%len(features)])
		}

		fmt.Fprintf(&b, "  - name: %s\n", s.Name)
		fmt.Fprintf(&b, "    chart: %s\n", s.Name)
		fmt.Fprintf(&b, "    namespace: %s\n", s.Layer)
		if len(requires) > 0 {
			fmt.Fprintf(&b, "    requires: [%s]\n", strings.Join(quoteAll(requires), ", "))
		}
		if len(dependsOn) > 0 {
			fmt.Fprintf(&b, "    dependsOn: [%s]\n", strings.Join(quoteAll(dedupe(dependsOn)), ", "))
		}
		fmt.Fprintf(&b, "    values:\n")
		fmt.Fprintf(&b, "      replicaCount: %d\n", 1+s.Index%3)
		fmt.Fprintf(&b, "      serviceAccount:\n        create: true\n")
		fmt.Fprintf(&b, "    exports:\n")
		fmt.Fprintf(&b, "      endpoint: '{{ .Release.Name }}-%s.%s.svc.cluster.local:8080'\n", s.Name, s.Layer)
		fmt.Fprintf(&b, "      serviceName: '{{ .Release.Name }}-%s'\n", s.Name)
	}
	return b.String()
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
