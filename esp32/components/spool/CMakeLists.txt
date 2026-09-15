idf_component_register(
    SRCS "src/spool.c" "src/spool_ring.c"
    INCLUDE_DIRS "include"
    PRIV_REQUIRES freertos heap esp_psram
)
