FROM node:lts-alpine3.13 AS builder
RUN apk add --no-cache git bash
WORKDIR /arby
ADD src.tar.gz .
RUN npm install

FROM node:lts-alpine3.13
RUN apk add --no-cache bash supervisor curl rsync
RUN mkdir /root/.arby
VOLUME [ "/root/.arby" ]
COPY --from=builder /arby /app
COPY entrypoint.sh /app
COPY supervisord.conf /etc/supervisor/conf.d/supervisord.conf
WORKDIR /app
ENTRYPOINT ["/usr/bin/supervisord", "-c", "/etc/supervisor/conf.d/supervisord.conf"]
