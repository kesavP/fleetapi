# Standalone package: build with this directory as the context.
#
# Stage 1 builds the app with Node. Stage 2 is nginx (the unprivileged image, uid 101, port 8080) serving the
# result and proxying the read API. Nothing of Node is in the final image.
FROM node:24-alpine AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY nginx/security-headers.conf /etc/nginx/snippets/security-headers.conf
COPY nginx/default.conf.template /etc/nginx/templates/default.conf.template
COPY nginx/05-resolver.envsh /docker-entrypoint.d/05-resolver.envsh
COPY --from=build /src/dist /usr/share/nginx/html
# Only these two are substituted into the template: the nginx variables in it ($api, $uri, ...) stay as they are.
ENV API_UPSTREAM=http://telemetry-ingest \
    ALERTS_UPSTREAM=http://telemetry-alerts \
    NGINX_ENVSUBST_FILTER='^(API_UPSTREAM|ALERTS_UPSTREAM|NGINX_RESOLVER)$'
EXPOSE 8080
