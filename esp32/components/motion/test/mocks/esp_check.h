#pragma once
#define ESP_RETURN_ON_ERROR(expr, tag, ...) do { \
    esp_err_t err_ = (expr); if (err_ != ESP_OK) return err_; \
} while (0)
#define ESP_GOTO_ON_ERROR(expr, goto_tag, tag, ...) do { \
    ret = (expr); if (ret != ESP_OK) goto goto_tag; \
} while (0)
