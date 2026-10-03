# mcpload-demo-nginx: stock nginx with the demo load balancer configs baked in,
# so the published demo (`mcpload demo up`) needs no files on the host.
# Each LB service picks its config: nginx -c /etc/nginx/mcpload/<name>.conf
# (docker-compose.yml here mounts the same files instead, for development.)
FROM nginx:1.29-alpine
COPY lb-stateful.conf stateless-2026.conf skew.conf skew-hang.conf /etc/nginx/mcpload/
