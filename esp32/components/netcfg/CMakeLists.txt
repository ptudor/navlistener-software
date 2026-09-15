idf_component_register(
    SRCS "src/netcfg.c" "src/netcfg_storage.c" "src/netcfg_check.c" "src/netcfg_form.c"
    INCLUDE_DIRS "include"
    PRIV_REQUIRES nvs_flash esp_wifi esp_netif esp_event esp_http_server bootloader_support esp_hw_support ota
)
