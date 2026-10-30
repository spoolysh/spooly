FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETPLATFORM

WORKDIR /

COPY ${TARGETPLATFORM}/boxy /boxy
COPY public/ /public/

ENTRYPOINT ["/boxy"]

CMD ["serve"]
