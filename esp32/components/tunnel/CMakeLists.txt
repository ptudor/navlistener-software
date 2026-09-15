# tunnel — WireGuard uplink to the collector. Only the ESP32-S3 observer enables
# NVF_WIREGUARD; other targets compile the stub half of src/tunnel.c, which never
# references the port. The port itself is the esphome/wireguard registry component
# declared in idf_component.yml, resolved for every target so dependencies.lock pins
# one revision regardless of which board was built last.
# Only the ESP32-S3 (NVF_WIREGUARD) links the WireGuard port; other targets compile the
# stub half of src/tunnel.c and must not depend on it, so the dependency is added to
# PRIV_REQUIRES only when the config is on. idf_component.yml gates the registry download
# to the S3 target to match, keeping the C6 image and its lock free of WireGuard.
set(tunnel_priv_requires esp_netif esp_event esp_wifi lwip freertos)
if(CONFIG_NVF_WIREGUARD)
    list(APPEND tunnel_priv_requires esphome__wireguard)
endif()
idf_component_register(
    SRCS "src/tunnel.c"
    INCLUDE_DIRS "include"
    REQUIRES netcfg
    PRIV_REQUIRES ${tunnel_priv_requires}
)
