idf_component_register(
    SRCS "src/journal_store.c" "src/journal.c"
    INCLUDE_DIRS "include"
    REQUIRES nvs_flash ubx observer_report
    PRIV_REQUIRES esp_timer esp_app_format app_update esp_wifi rtc spool pusher
)
