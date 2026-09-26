#pragma once
#define ESP_RETURN_ON_ERROR(expr, tag, ...) do { \
    esp_err_t err_ = (expr); if (err_ != ESP_OK) return err_; \
} while (0)
