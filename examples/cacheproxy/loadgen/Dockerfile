# The load generator beside the proxy: it shares vis-cacheproxy's network, so its
# requests reach the proxy over loopback inside the VM instead of through the
# host's port forwarding, which can't carry hundreds of slow connections for long.
FROM scratch
COPY loadgen /loadgen
ENTRYPOINT ["/loadgen", "-target", "http://127.0.0.1:8790", "-control", "0.0.0.0:8792"]
