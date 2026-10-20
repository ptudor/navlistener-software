idf_component_register(
    SRCS "src/environment.c" "src/env_sensors.c" "src/mcp9808.c" "src/env_heater.c" "src/ms5607.c" "src/mmc34160.c"
         "src/bmp5.c" "src/ina3221.c" "src/max31856.c" "src/thermocouple.c" "src/sht3x.c" "src/hih8121.c"
         "vendor/bmp3/bmp3.c"
    INCLUDE_DIRS "include" "vendor/bmp3"
    REQUIRES esp_driver_i2c
    PRIV_REQUIRES nvs_flash esp_driver_spi esp_driver_gpio
)
