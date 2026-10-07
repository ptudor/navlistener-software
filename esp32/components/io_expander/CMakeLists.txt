# io_expander — the 162mm board's MCP23008 (U39). src/io_expander.c is pure C with host tests
# in test/; src/io_expander_service.c is the FreeRTOS task and interrupt glue.
idf_component_register(
    SRCS "src/io_expander.c" "src/io_expander_service.c"
    INCLUDE_DIRS "include"
    REQUIRES esp_driver_i2c
    PRIV_REQUIRES esp_driver_gpio esp_timer
)
