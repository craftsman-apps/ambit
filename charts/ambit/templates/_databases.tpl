{{/* Where ClickHouse and Valkey are: the chart's own, or external ones (evidence.clickhouse.external,
limits.valkey.external). */}}
{{- define "ambit.clickhouse.external" -}}{{ if .Values.evidence.clickhouse.external.host }}true{{ end }}{{- end -}}
{{- define "ambit.clickhouse.http" -}}
{{- with .Values.evidence.clickhouse.external -}}
{{- if .host }}{{ .host }}:{{ .httpPort }}{{ else }}ambit-clickhouse:8123{{ end -}}
{{- end -}}
{{- end -}}
{{- define "ambit.clickhouse.native" -}}
{{- with .Values.evidence.clickhouse.external -}}
{{- if .host }}tcp://{{ .host }}:{{ .nativePort }}?dial_timeout=10s{{ if .tls }}&secure=true{{ end }}{{ else }}tcp://ambit-clickhouse:9000?dial_timeout=10s{{ end -}}
{{- end -}}
{{- end -}}
{{- define "ambit.clickhouse.passwordSecret" -}}
{{- with .Values.evidence.clickhouse.external -}}
{{- if .host }}{{ required "evidence.clickhouse.external.passwordSecret is required with an external ClickHouse" .passwordSecret }}{{ else }}ambit-clickhouse{{ end -}}
{{- end -}}
{{- end -}}
{{- define "ambit.valkey.address" -}}
{{- .Values.limits.valkey.external.address | default "ambit-valkey:6379" -}}
{{- end -}}
{{/* A CA is mounted only for an external database reached over TLS (the exporter turns TLS on whenever
it has a CA file, whatever the endpoint says). */}}
{{- define "ambit.clickhouse.ca" -}}{{ with .Values.evidence.clickhouse.external }}{{ if and .host .tls .caSecret }}{{ .caSecret }}{{ end }}{{ end }}{{- end -}}
{{- define "ambit.valkey.ca" -}}{{ with .Values.limits.valkey.external }}{{ if and .address .tls .caSecret }}{{ .caSecret }}{{ end }}{{ end }}{{- end -}}
