FROM ghcr.io/merkat0r/go-rflink:latest
USER root
RUN apk add --no-cache jq
COPY run.sh /run.sh
RUN chmod 0755 /run.sh
CMD ["/run.sh"]
