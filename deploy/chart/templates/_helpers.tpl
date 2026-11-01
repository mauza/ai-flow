{{- define "ai-flow.labels" -}}
app.kubernetes.io/name: ai-flow
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "ai-flow.selector" -}}
app.kubernetes.io/name: ai-flow
app.kubernetes.io/component: control-plane
{{- end }}

{{- define "ai-flow.garageSecret" -}}
{{ .Values.garage.existingSecret | default "garage" }}
{{- end }}

{{/*
The control-plane image. A tag must be a string: an all-digit short sha
written unquoted (tag: 3295049) is a YAML number and renders as
3.295049e+06, which took the control plane down. Fail the render instead,
so the running release stays up and Argo shows why.
*/}}
{{- define "ai-flow.image" -}}
{{- if not (kindIs "string" .Values.image.tag) -}}
{{- fail (printf "image.tag must be a quoted string, got %v (a %s): write tag: \"%v\"" .Values.image.tag (kindOf .Values.image.tag) .Values.image.tag) -}}
{{- end -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end }}
