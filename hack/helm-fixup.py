#!/usr/bin/env python3
"""Repair the generated Helm chart.

kubebuilder's helm/v2-alpha plugin assumes a project has exactly one
ServiceAccount and one image. Runway has two of each (controller and portal),
so the plugin drops the controller's ServiceAccount, templates the portal's
as if it were the controller's, and leaves the portal image hard-coded.
`make helm-chart` runs this right after the plugin to put those three right.
"""
import pathlib
import sys

chart = pathlib.Path(__file__).resolve().parent.parent / "dist" / "chart" / "templates"

(chart / "rbac" / "controller-manager-sa.yaml").write_text("""{{- if .Values.serviceAccount.enabled }}
apiVersion: v1
kind: ServiceAccount
metadata:
  {{- with .Values.serviceAccount.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.serviceAccount.labels }}
  labels:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  name: {{ include "runway.serviceAccountName" . }}
  namespace: {{ .Release.Namespace }}
{{- end }}
""")

(chart / "rbac" / "portal.yaml").write_text("""apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "runway.resourceName" (dict "suffix" "portal" "context" $) }}
  namespace: {{ .Release.Namespace }}
""")

portal = chart / "extras" / "portal-server.yaml"
text = portal.read_text()
image = ('image: "{{ .Values.manager.image.repository | default "controller" }}'
         '{{- if not (contains "@" (.Values.manager.image.repository | default "controller")) }}'
         ':{{ .Values.manager.image.tag | default .Chart.AppVersion }}{{- end }}"')
if "image: controller:latest" not in text and image not in text:
    sys.exit("helm-fixup: portal image line not found; the plugin's output changed")
text = text.replace("image: controller:latest", image)
sa = 'serviceAccountName: {{ include "runway.resourceName" (dict "suffix" "portal" "context" $) }}'
text = text.replace("serviceAccountName: runway-portal", sa)
if sa not in text:
    sys.exit("helm-fixup: portal serviceAccountName line not found; the plugin's output changed")
portal.write_text(text)
print("helm-fixup: chart repaired")
