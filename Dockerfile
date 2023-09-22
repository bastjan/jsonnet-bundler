FROM docker.io/library/alpine:3.17 as runtime

RUN \
  apk add --update --no-cache \
    bash \
    curl \
    git \
    ca-certificates \
    tzdata

# TODO: Adjust binary file name
ENTRYPOINT ["jb"]
COPY jb /usr/bin/

USER 65536:0
