{{/*
OCI extra Service ports and extra Ingress hosts are derived from
provisioning.document repository endpoints so Helm values are not a second
copy of the process configuration. service.extraPorts and ingress.extraHosts
remain only for existingConfigMap / API-managed repositories.
*/}}

{{- define "suxen.provisioningResources" -}}
{{- $resources := list -}}
{{- if .Values.provisioning.document -}}
{{- $asMap := .Values.provisioning.document | fromYaml -}}
{{- if and (kindIs "map" $asMap) $asMap.Error -}}
{{- $asList := .Values.provisioning.document | fromYamlArray -}}
{{- if kindIs "slice" $asList -}}
{{- $resources = $asList -}}
{{- else -}}
{{- fail (printf "provisioning.document is not valid YAML: %s" $asMap.Error) -}}
{{- end -}}
{{- else if and (kindIs "map" $asMap) $asMap.resources -}}
{{- $resources = $asMap.resources -}}
{{- else if and (kindIs "map" $asMap) $asMap.kind -}}
{{- $resources = list $asMap -}}
{{- else -}}
{{- $asList := .Values.provisioning.document | fromYamlArray -}}
{{- if kindIs "slice" $asList -}}
{{- $resources = $asList -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- dict "items" $resources | toJson -}}
{{- end }}

{{- define "suxen.ociDerivedEndpoints" -}}
{{- $primary := int (include "suxen.httpPort" .) -}}
{{- $ports := dict -}}
{{- $hosts := dict -}}
{{- range $resource := (include "suxen.provisioningResources" . | fromJson).items -}}
{{- if and (eq $resource.kind "repository") $resource.spec (eq (default "" $resource.spec.format) "oci") -}}
{{- $endpoints := default dict $resource.spec.endpoints -}}
{{- range $endpoints.ports -}}
{{- $port := int . -}}
{{- if and (gt $port 0) (ne $port $primary) -}}
{{- $_ := set $ports (toString $port) $port -}}
{{- end -}}
{{- end -}}
{{- range $endpoints.hosts -}}
{{- if . -}}
{{- $_ := set $hosts . true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $portList := list -}}
{{- range $key := keys $ports | sortAlpha -}}
{{- $portList = append $portList (dict "name" (printf "oci-%s" $key) "port" (index $ports $key)) -}}
{{- end -}}
{{- dict "ports" $portList "hosts" (keys $hosts | sortAlpha) | toJson -}}
{{- end }}

{{- define "suxen.ociServicePorts" -}}
{{- $derived := include "suxen.ociDerivedEndpoints" . | fromJson -}}
{{- if gt (len $derived.ports) 0 -}}
{{- dict "items" $derived.ports | toJson -}}
{{- else -}}
{{- dict "items" (.Values.service.extraPorts | default list) | toJson -}}
{{- end -}}
{{- end }}

{{- define "suxen.ingressMainHosts" -}}
{{- $hosts := dict -}}
{{- range .Values.ingress.hosts -}}
{{- if .host -}}
{{- $_ := set $hosts .host true -}}
{{- end -}}
{{- end -}}
{{- $hosts | toJson -}}
{{- end }}

{{- define "suxen.validateOCIEndpoints" -}}
{{- $derived := include "suxen.ociDerivedEndpoints" . | fromJson -}}
{{- $derivedPorts := default list $derived.ports -}}
{{- $derivedHosts := default list $derived.hosts -}}
{{- $hasDerived := or (gt (len $derivedPorts) 0) (gt (len $derivedHosts) 0) -}}
{{- if and $hasDerived (gt (len .Values.service.extraPorts) 0) -}}
{{- fail "service.extraPorts is derived from provisioning.document repository endpoints.ports; remove service.extraPorts" -}}
{{- end -}}
{{- if and $hasDerived (gt (len .Values.ingress.extraHosts) 0) -}}
{{- fail "ingress.extraHosts is derived from provisioning.document repository endpoints.hosts; remove ingress.extraHosts and set ingress.ociHosts only for TLS or annotations" -}}
{{- end -}}
{{- $known := dict -}}
{{- range $derivedHosts -}}
{{- $_ := set $known . true -}}
{{- end -}}
{{- range $host, $cfg := .Values.ingress.ociHosts -}}
{{- if not (index $known $host) -}}
{{- fail (printf "ingress.ociHosts[%q] has no matching provisioning.document repository endpoints.hosts entry" $host) -}}
{{- end -}}
{{- end -}}
{{- range (include "suxen.ociServicePorts" . | fromJson).items -}}
{{- if eq (int .port) (int $.Values.service.port) -}}
{{- fail (printf "OCI extra Service port %v conflicts with service.port" .port) -}}
{{- end -}}
{{- end -}}
{{- end }}
