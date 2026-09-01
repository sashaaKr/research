package main

import (
	"fmt"
	"strings"
)

// Jinja dialect of the same chart library.
//
// These templates exist to make a Go-vs-Rust comparison honest. Helm charts are
// Go text/template plus sprig, and no Rust library renders them, so a Rust port
// cannot render the real thing. What it *can* do is render the same work: the
// same values, the same number of template actions, the same helper calls, the
// same output.
//
// Each template below is a line-for-line counterpart of its Go original in
// main.go, written so both engines emit byte-identical bytes for identical
// input. That equivalence is asserted by a test, not assumed - without it the
// benchmark would be comparing two different amounts of work and the numbers
// would mean nothing.
//
// Two deliberate structural differences, neither of which changes the work:
//
//   - Go's `define`/`include` becomes Jinja `macro`/call. Both are "render a
//     named fragment and splice the result", and both cost a nested execution.
//   - Go's implicit `.` context becomes an explicit `ctx` argument, because
//     Jinja macros do not inherit the caller's scope.

const jinjaHelpers = `{% macro chart_name(ctx) %}{{ (ctx.Values.nameOverride or ctx.Chart.Name) | trunc(63) | trim_suffix("-") }}{% endmacro %}
{% macro chart_fullname(ctx) %}{% if ctx.Values.fullnameOverride %}{{ ctx.Values.fullnameOverride | trunc(63) | trim_suffix("-") }}{% else %}{{ (ctx.Release.Name ~ "-" ~ chart_name(ctx)) | trunc(63) | trim_suffix("-") }}{% endif %}{% endmacro %}
{% macro chart_chart(ctx) %}{{ (ctx.Chart.Name ~ "-" ~ ctx.Chart.Version) | replace("+", "_") | trunc(63) | trim_suffix("-") }}{% endmacro %}
{% macro chart_labels(ctx) %}helm.sh/chart: {{ chart_chart(ctx) }}
app.kubernetes.io/name: {{ chart_name(ctx) }}
app.kubernetes.io/instance: {{ ctx.Release.Name }}
app.kubernetes.io/version: {{ ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ ctx.Release.Service }}
hedp.example.com/customer: {{ (ctx.Values.customer.id or "unknown") | quote }}
hedp.example.com/tier: {{ (ctx.Values.customer.tier or "none") | quote }}{% if ctx.Values.extraLabels %}
{{- ctx.Values.extraLabels | to_yaml }}{% endif %}{% endmacro %}
{% macro chart_selector_labels(ctx) %}app.kubernetes.io/name: {{ chart_name(ctx) }}
app.kubernetes.io/instance: {{ ctx.Release.Name }}{% endmacro %}
{% macro chart_service_account_name(ctx) %}{% if ctx.Values.serviceAccount.create %}{{ ctx.Values.serviceAccount.name or chart_fullname(ctx) }}{% else %}{{ ctx.Values.serviceAccount.name or "default" }}{% endif %}{% endmacro %}
`

const jinjaDeployment = `{% import "_helpers.j2" as h %}apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ h.chart_fullname(ctx) }}
  labels:
    {{- h.chart_labels(ctx) | nindent(4) }}
spec:
{%- if not ctx.Values.autoscaling.enabled %}
  replicas: {{ ctx.Values.replicaCount }}
{%- endif %}
  selector:
    matchLabels:
      {{- h.chart_selector_labels(ctx) | nindent(6) }}
  template:
    metadata:
      annotations:
        checksum/config: {{ ctx.Values.customer | to_yaml | sha256sum }}
      labels:
        {{- h.chart_selector_labels(ctx) | nindent(8) }}
    spec:
      serviceAccountName: {{ h.chart_service_account_name(ctx) }}
      containers:
        - name: {{ ctx.Chart.Name }}
          image: "{{ ctx.Values.image.repository }}:{{ ctx.Values.image.tag or ctx.Chart.AppVersion }}"
          imagePullPolicy: {{ ctx.Values.image.pullPolicy }}
          ports:
            - name: http
              containerPort: {{ ctx.Values.service.port }}
              protocol: TCP
          env:
            - name: CUSTOMER_ID
              value: {{ ctx.Values.customer.id | quote }}
            - name: REGION
              value: {{ ctx.Values.customer.region | quote }}
{%- for name, dep in ctx.Values.deps | dictsort %}
            - name: {{ name | upper | replace("-", "_") }}_ENDPOINT
              value: {{ dep.endpoint | quote }}
{%- endfor %}
{%- for e in ctx.Values.env %}
            - name: {{ e.name }}
              value: {{ e.value | quote }}
{%- endfor %}
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
          resources:
            {{- ctx.Values.resources | to_yaml | nindent(12) }}
{%- if ctx.Values.nodeSelector %}
      nodeSelector:
        {{- ctx.Values.nodeSelector | to_yaml | nindent(8) }}
{%- endif %}
{%- if ctx.Values.tolerations %}
      tolerations:
        {{- ctx.Values.tolerations | to_yaml | nindent(8) }}
{%- endif %}
{%- if ctx.Values.affinity %}
      affinity:
        {{- ctx.Values.affinity | to_yaml | nindent(8) }}
{%- endif %}
`

const jinjaService = `{% import "_helpers.j2" as h %}apiVersion: v1
kind: Service
metadata:
  name: {{ h.chart_fullname(ctx) }}
  labels:
    {{- h.chart_labels(ctx) | nindent(4) }}
spec:
  type: {{ ctx.Values.service.type }}
  ports:
    - port: {{ ctx.Values.service.port }}
      targetPort: http
      protocol: TCP
      name: http
  selector:
    {{- h.chart_selector_labels(ctx) | nindent(4) }}
`

const jinjaConfigmap = `{% import "_helpers.j2" as h %}apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ h.chart_fullname(ctx) }}-config
  labels:
    {{- h.chart_labels(ctx) | nindent(4) }}
data:
  customer.yaml: |
    {{- ctx.Values.customer | to_yaml | nindent(4) }}
  upstreams.yaml: |
{%- if ctx.Values.deps %}
    {{- ctx.Values.deps | to_yaml | nindent(4) }}
{%- else %}
    {}
{%- endif %}
`

const jinjaIngress = `{% import "_helpers.j2" as h %}{% if ctx.Values.ingress.enabled %}
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ h.chart_fullname(ctx) }}
  labels:
    {{- h.chart_labels(ctx) | nindent(4) }}
spec:
  ingressClassName: {{ ctx.Values.ingress.className }}
  rules:
{%- for rule in ctx.Values.ingress.hosts %}
    - host: {{ rule.host | quote }}
      http:
        paths:
          - path: {{ rule.path or "/" }}
            pathType: Prefix
            backend:
              service:
                name: {{ h.chart_fullname(ctx) }}
                port:
                  number: {{ ctx.Values.service.port }}
{%- endfor %}
{%- endif %}
`

const jinjaHpa = `{% import "_helpers.j2" as h %}{% if ctx.Values.autoscaling.enabled %}
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ h.chart_fullname(ctx) }}
  labels:
    {{- h.chart_labels(ctx) | nindent(4) }}
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: {{ h.chart_fullname(ctx) }}
  minReplicas: {{ ctx.Values.autoscaling.minReplicas }}
  maxReplicas: {{ ctx.Values.autoscaling.maxReplicas }}
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: {{ ctx.Values.autoscaling.targetCPUUtilizationPercentage }}
{%- endif %}
`

const jinjaRbac = `{% import "_helpers.j2" as h %}{% if ctx.Values.serviceAccount.create %}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ h.chart_service_account_name(ctx) }}
  labels:
    {{- h.chart_labels(ctx) | nindent(4) }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ h.chart_fullname(ctx) }}
  labels:
    {{- h.chart_labels(ctx) | nindent(4) }}
rules:
  - apiGroups: [""]
    resources: ["configmaps", "secrets"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ h.chart_fullname(ctx) }}
  labels:
    {{- h.chart_labels(ctx) | nindent(4) }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ h.chart_fullname(ctx) }}
subjects:
  - kind: ServiceAccount
    name: {{ h.chart_service_account_name(ctx) }}
    namespace: {{ ctx.Release.Namespace }}
{%- endif %}
`

// jinjaAssets mirrors the .Files.Glob template. The Rust side exposes the
// chart's non-template files as a map under `files`, which is the same
// "shipped with the chart, read but never parsed" bucket.
const jinjaAssets = `{% import "_helpers.j2" as h %}{% if ctx.Values.assets.enabled %}
{%- for path, body in ctx.files | dictsort %}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ h.chart_fullname(ctx) }}-{{ path | basename | trim_suffix(".json") }}
  labels:
    {{- h.chart_labels(ctx) | nindent(4) }}
data:
  {{ path | basename }}: |
    {{- body | nindent(4) }}
{%- endfor %}
{%- endif %}
`

// jinjaDashboardWithPanels mirrors dashboardWithPanels: the same dashboard,
// the same panel count, the same static tokens, the same template actions per
// panel. Only the syntax differs.
func jinjaDashboardWithPanels(chartName string, idx, panels int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{%% import "_helpers.j2" as h %%}apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ h.chart_fullname(ctx) }}-dash-%03d
  labels:
    {{- h.chart_labels(ctx) | nindent(4) }}
  annotations:
    hedp.example.com/chart: %q
data:
  dashboard-%03d.json: |
    {
      "title": "{{ ctx.Values.customer.id }} / %s / panel %d",
      "tags": [{%% for f in ctx.Values.customer.features %%}{%% if not loop.first %%}, {%% endif %%}{{ f | quote }}{%% endfor %%}],
      "panels": [
`, idx, chartName, idx, chartName, idx)

	for panel := 0; panel < panels; panel++ {
		fmt.Fprintf(&b, `        {
          "id": %d,
          "type": %q,
          "title": "{{ ctx.Values.customer.tier }} panel %d",
          "datasource": {"uid": "{{ ctx.Release.Name }}-ds"},
          "targets": [{"expr": "sum(rate(http_requests_total{job=\"%s\",instance=\"%s\"}[5m]))", "refId": "A"}],
          "fieldConfig": {"defaults": {"unit": "reqps", "custom": {"lineWidth": %d, "fillOpacity": %d}}},
          "gridPos": {"h": 8, "w": 12, "x": %d, "y": %d}
        },
`, panel, panelTypes[panel%len(panelTypes)], panel, chartName, dashToken(chartName, idx, panel),
			1+panel%3, panel%40, (panel%2)*12, (panel/2)*8)
	}

	b.WriteString(`        {"id": 9999, "type": "row", "title": "end"}
      ]
    }
`)
	return b.String()
}

// jinjaFixed maps the Go template filenames to their Jinja counterparts. The
// names are kept aligned so a diff of the two libraries lines up.
func jinjaFixed() map[string]string {
	return map[string]string{
		"templates/_helpers.j2":   jinjaHelpers,
		"templates/deployment.j2": jinjaDeployment,
		"templates/service.j2":    jinjaService,
		"templates/configmap.j2":  jinjaConfigmap,
		"templates/ingress.j2":    jinjaIngress,
		"templates/hpa.j2":        jinjaHpa,
		"templates/rbac.j2":       jinjaRbac,
		"templates/assets.j2":     jinjaAssets,
	}
}
