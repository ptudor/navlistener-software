# oled — the status display on a board's display header: a 128x64 SSD1306 module on the
# shared I2C bus. The canvas, the font, the pages, the receiver adapter and the SSD1306
# protocol are pure C with host tests in test/; src/oled_service.c is the FreeRTOS task and
# the I2C glue.
idf_component_register(
    SRCS "src/oled_canvas.c" "src/oled_font.c" "src/oled_pages.c" "src/oled_gnss.c" "src/ssd1306.c"
         "src/oled_service.c"
    INCLUDE_DIRS "include"
    REQUIRES esp_driver_i2c ubx
    PRIV_REQUIRES nvs_flash esp_timer
)
