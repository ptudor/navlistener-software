idf_component_register(
    SRCS "src/pusher.c" "src/ack_progress.c"
    INCLUDE_DIRS "include"
    REQUIRES gnf1 spool
    PRIV_REQUIRES esp-tls esp_timer freertos lwip
)
