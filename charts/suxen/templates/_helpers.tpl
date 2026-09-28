{{- define "suxen.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "suxen.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "suxen.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "suxen.labels" -}}
app.kubernetes.io/name: {{ include "suxen.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{- define "suxen.selectorLabels" -}}
app.kubernetes.io/name: {{ include "suxen.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "suxen.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{- define "suxen.httpPort" -}}8080{{- end }}

{{- define "suxen.validateScaling" -}}
{{- if and .Values.provisioning.enabled (not (or .Values.provisioning.document .Values.provisioning.existingConfigMap)) -}}
{{- fail "provisioning.enabled requires provisioning.document or provisioning.existingConfigMap" -}}
{{- end -}}
{{- if and .Values.provisioning.document .Values.provisioning.existingConfigMap -}}
{{- fail "provisioning.document and provisioning.existingConfigMap are mutually exclusive" -}}
{{- end -}}
{{- range .Values.provisioning.secretMounts -}}
{{- if not (hasPrefix "/" .mountPath) -}}
{{- fail "provisioning.secretMounts[].mountPath must be absolute" -}}
{{- end -}}
{{- end -}}
{{- $scaled := or (gt (int .Values.replicaCount) 1) .Values.autoscaling.enabled -}}
{{- $sharedDatabase := or .Values.database.bundled.enabled .Values.database.existingSecret (hasPrefix "postgres" .Values.database.url) -}}
{{- /* $sharedBlobs is an external object store, which also selects ephemeral pod
       storage instead of the chart-managed PVC. */ -}}
{{- $sharedBlobs := or .Values.blobStore.existingSecret (hasPrefix "s3://" .Values.blobStore.url) (hasPrefix "gcs://" .Values.blobStore.url) -}}
{{- /* $clusterSafeBlobs additionally accepts the chart PVC when the operator
       vouches for it as ReadWriteMany via blobStore.shared. */ -}}
{{- $clusterSafeBlobs := or $sharedBlobs .Values.blobStore.shared -}}
{{- $rwxPersistence := and .Values.persistence.enabled (has "ReadWriteMany" .Values.persistence.accessModes) -}}
{{- if and .Values.blobStore.shared $sharedBlobs -}}
{{- fail "blobStore.shared applies to the filesystem PVC and cannot be combined with an object or existingSecret blob store" -}}
{{- end -}}
{{- if and .Values.blobStore.shared (not $rwxPersistence) -}}
{{- fail "blobStore.shared=true requires persistence.enabled with persistence.accessModes including ReadWriteMany" -}}
{{- end -}}
{{- if and .Values.blobStore.gcsCredentialsSecret .Values.blobStore.url (not (hasPrefix "gcs://" .Values.blobStore.url)) -}}
{{- fail "blobStore.gcsCredentialsSecret requires a gcs:// blobStore.url" -}}
{{- end -}}
{{- if and .Values.blobStore.gcsCredentialsSecret (not .Values.blobStore.gcsCredentialsKey) -}}
{{- fail "blobStore.gcsCredentialsSecret requires blobStore.gcsCredentialsKey" -}}
{{- end -}}
{{- if and .Values.blobStore.credentialsSecret .Values.blobStore.gcsCredentialsSecret -}}
{{- fail "blobStore.credentialsSecret and blobStore.gcsCredentialsSecret are mutually exclusive" -}}
{{- end -}}
{{- if and $scaled (not .Values.config.cluster) -}}
{{- fail "replicaCount > 1 or autoscaling requires config.cluster=true" -}}
{{- end -}}
{{- if and $scaled (not $sharedDatabase) -}}
{{- fail "replicaCount > 1 or autoscaling requires a Postgres database" -}}
{{- end -}}
{{- if and $scaled (not $clusterSafeBlobs) -}}
{{- fail "replicaCount > 1 or autoscaling requires an S3-compatible or GCS blob store, or blobStore.shared=true on a ReadWriteMany filesystem volume" -}}
{{- end -}}
{{- if and .Values.config.cluster (not $sharedDatabase) -}}
{{- fail "config.cluster=true requires a Postgres database" -}}
{{- end -}}
{{- if and .Values.config.cluster (not $clusterSafeBlobs) -}}
{{- fail "config.cluster=true requires an S3-compatible or GCS blob store, or blobStore.shared=true on a ReadWriteMany filesystem volume" -}}
{{- end -}}
{{- $distributed := or .Values.config.cluster $sharedDatabase $clusterSafeBlobs -}}
{{- if and $distributed (not .Values.bootstrap.existingSecret) -}}
{{- fail "Postgres, object-storage, or cluster deployments require bootstrap.existingSecret" -}}
{{- end -}}
{{- if and .Values.serviceMonitor.enabled (not .Values.serviceMonitor.bearerTokenSecret.name) -}}
{{- fail "serviceMonitor.enabled requires serviceMonitor.bearerTokenSecret.name because /metrics requires authentication" -}}
{{- end -}}
{{- if and .Values.serviceMonitor.enabled (not .Values.serviceMonitor.bearerTokenSecret.key) -}}
{{- fail "serviceMonitor.enabled requires serviceMonitor.bearerTokenSecret.key" -}}
{{- end -}}
{{- end }}
