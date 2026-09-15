idf_component_register(
    SRCS "src/pulse_stats.c" "src/pulse_timing.c"
    INCLUDE_DIRS "include"
    REQUIRES observer_report
    PRIV_REQUIRES esp_driver_mcpwm esp_driver_pcnt esp_driver_gpio esp_timer
)
