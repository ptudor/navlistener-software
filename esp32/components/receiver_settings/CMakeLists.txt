# receiver_settings — per-unit receiver settings in NVS (include/receiver_settings.h).
idf_component_register(
    SRCS "src/receiver_settings.c"
    INCLUDE_DIRS "include"
    PRIV_REQUIRES nvs_flash
)
