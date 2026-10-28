idf_component_register(SRCS "src/observer_report.c" "src/panel_control.c" "src/panel_settings.c" "src/panel_boot.c"
    INCLUDE_DIRS "include" REQUIRES gnf1 PRIV_REQUIRES nvs_flash)
