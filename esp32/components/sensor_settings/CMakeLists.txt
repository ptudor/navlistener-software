# sensor_settings — per-unit sensor settings in NVS (include/sensor_settings.h).
idf_component_register(
    SRCS "src/sensor_settings.c"
    INCLUDE_DIRS "include"
    PRIV_REQUIRES nvs_flash
)
