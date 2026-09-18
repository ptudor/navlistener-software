# The portable core (mcu_identity_core.c) has no ESP-IDF dependency and is host-tested in
# test/; mcu_identity.c is the ESP32-S3 layer over the Digital Signature peripheral.
idf_component_register(
    SRCS "src/mcu_identity_core.c" "src/mcu_identity.c"
    INCLUDE_DIRS "include"
    PRIV_REQUIRES esp_security esp_hw_support efuse bootloader_support mbedtls nvs_flash esp_wifi journal
)
