# tunnel — WireGuard uplink to the collector. Only the ESP32-S3 observer enables
# NVF_WIREGUARD; other targets compile the stub half of src/tunnel.c, which never
# references the port. The port itself is the esphome/wireguard registry component
# declared and pinned in idf_component.yml.
# Only NVF_WIREGUARD links the WireGuard port; builds with it disabled compile the
# stub half of src/tunnel.c and must not depend on it, so the dependency is added to
# PRIV_REQUIRES only when the config is on. idf_component.yml gates the registry download
# to the supported S3 target to match.
set(tunnel_priv_requires esp_netif lwip freertos)
if(CONFIG_NVF_WIREGUARD)
    list(APPEND tunnel_priv_requires esphome__wireguard)
endif()
idf_component_register(
    SRCS "src/tunnel.c"
    INCLUDE_DIRS "include"
    REQUIRES netcfg
    PRIV_REQUIRES ${tunnel_priv_requires}
)
