idf_component_register(
    SRCS "src/display_st7789.c"
    INCLUDE_DIRS "include"
    PRIV_REQUIRES driver esp_lcd esp_app_format
)
