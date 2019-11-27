FROM alpine
COPY ./bin/proxy /bin/proxy
COPY ./bin/client /bin/client
RUN mkdir -p /etc/cyral/cyral-push-proxy/

RUN GRPC_HEALTH_PROBE_VERSION=v0.2.1 && \
     wget -qO/bin/grpc_health_probe https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/${GRPC_HEALTH_PROBE_VERSION}/grpc_health_probe-linux-amd64 && \
        chmod +x /bin/grpc_health_probe

# Run the binary
ENTRYPOINT ["/bin/proxy"] 
