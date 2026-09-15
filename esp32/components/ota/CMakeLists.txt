idf_component_register(
    SRCS "src/ota.c" "src/ota_download.c" "src/ota_policy.c" "src/ota_image.c"
    INCLUDE_DIRS "include"
    REQUIRES esp_http_server
    PRIV_REQUIRES app_update esp_http_client mbedtls nvs_flash esp_wifi
                  esp_timer esp_netif lwip esp_app_format bootloader_support json spool journal
)
target_link_libraries(${COMPONENT_LIB} INTERFACE "-u nvf_ota_metadata")
