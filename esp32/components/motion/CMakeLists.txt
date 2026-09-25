# motion — the MAX board's ICM-45686 IMU. src/icm45686.c is pure C with host tests in
# test/; src/motion.c is the FreeRTOS service task and interrupt glue.
idf_component_register(
    SRCS "src/icm45686.c" "src/motion.c"
    INCLUDE_DIRS "include"
    REQUIRES esp_driver_i2c
    PRIV_REQUIRES esp_driver_gpio esp_timer
)
