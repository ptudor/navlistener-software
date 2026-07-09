idf_component_register(
    SRCS "main.c"
    INCLUDE_DIRS "."
    REQUIRES ubx gnf1 driver nvs_flash esp_timer
)
