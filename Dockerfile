FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETPLATFORM

WORKDIR /

COPY ${TARGETPLATFORM}/spooly /spooly
COPY public/ /public/

ENTRYPOINT ["/spooly"]

CMD ["serve"]
