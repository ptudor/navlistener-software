set(netcfg_sources
    "src/netcfg.c"
    "src/netcfg_storage.c"
    "src/netcfg_setup.c"
    "src/netcfg_check.c"
    "src/netcfg_form.c"
    "src/netcfg_tunnel.c"
)
set(netcfg_private_requires
    nvs_flash esp_wifi esp_netif esp_event esp_http_server
    bootloader_support esp_hw_support ota wifi_provisioning protocomm
)

if(CONFIG_NVF_BOARD_GNSS_COLOR)
    list(APPEND netcfg_sources "src/netcfg_ble.c" "src/netcfg_prov_payload.c")
endif()

idf_component_register(
    SRCS ${netcfg_sources}
    INCLUDE_DIRS "include"
    PRIV_REQUIRES ${netcfg_private_requires}
)
