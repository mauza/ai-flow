{{- define "ai-flow.labels" -}}
app.kubernetes.io/name: ai-flow
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "ai-flow.selector" -}}
app.kubernetes.io/name: ai-flow
app.kubernetes.io/component: control-plane
{{- end }}
