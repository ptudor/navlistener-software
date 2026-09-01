idf_component_register(
    SRCS "src/hardware_manifest.c" "src/hardware_manifest_policy.c"
    INCLUDE_DIRS "include"
    REQUIRES esp_hardware_discovery esp_driver_i2c nvs_flash
)
