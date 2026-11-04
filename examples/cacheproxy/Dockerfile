# A scratch image: just the cross-compiled binary, nothing to download.
# Build it with run-container.sh, which compiles for the daemon's architecture first.
FROM scratch
COPY cacheproxy /cacheproxy
EXPOSE 8790 7070 7071 7946
ENTRYPOINT ["/cacheproxy", "-addr", "0.0.0.0:8790", "-mcp", "0.0.0.0:7070", "-ui", "0.0.0.0:7071"]
