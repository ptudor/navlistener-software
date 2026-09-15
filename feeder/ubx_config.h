/* Volatile setup for receivers supporting UBX-CFG-VALSET (e.g. u-blox F9).
 * Included by navfeeder.c after its POSIX headers, logging and clock helpers.
 * Key IDs/layers: u-blox F9 TIM 2.20 Interface Description, UBX-21048598-R01.
 */
#ifndef NAVFEEDER_UBX_CONFIG_H
#define NAVFEEDER_UBX_CONFIG_H

#define UBX_CONFIG_RETRY_S 30
#define UBX_CONFIG_ACK_S 3

struct ubx_config {
	int port, awaiting_ack;
	time_t next_attempt, ack_deadline;
};

static int ubx_config_port(const char *name) {
	if (!strcmp(name, "uart1")) return 1;
	if (!strcmp(name, "uart2")) return 2;
	if (!strcmp(name, "usb")) return 3;
	return 0;
}

/* Exactly four single-byte keys: UBX output enable and three message rates.
 * No baud, NMEA, signal, timing, backup RAM or flash configuration is sent. */
static size_t ubx_config_packet(unsigned char out[32], int port) {
	static const uint32_t keys[3][4] = {
		{ 0x10740001, 0x20910232, 0x20910016, 0x2091035a },
		{ 0x10760001, 0x20910233, 0x20910017, 0x2091035b },
		{ 0x10780001, 0x20910234, 0x20910018, 0x2091035c },
	};
	if (port < 1 || port > 3) return 0;
	memset(out, 0, 32);
	out[0] = 0xb5; out[1] = 0x62; out[2] = 0x06; out[3] = 0x8a;
	out[4] = 24; /* payload length */
	out[6] = 0; out[7] = 1; /* version 0, layers = RAM only */
	for (unsigned i = 0; i < 4; i++) {
		uint32_t key = keys[port - 1][i];
		for (unsigned j = 0; j < 4; j++) out[10 + i*5 + j] = (unsigned char)(key >> (j*8));
		out[14 + i*5] = 1;
	}
	unsigned char a = 0, b = 0;
	for (unsigned i = 2; i < 30; i++) { a += out[i]; b += a; }
	out[30] = a; out[31] = b;
	return 32;
}

/* Bound writes even if a serial driver stops accepting bytes. Leave the fd's
 * read mode as it was; never flush pending input or wait in an unbounded drain. */
static int ubx_config_write(int fd, const unsigned char *buf, size_t len) {
	int flags = fcntl(fd, F_GETFL);
	if (flags < 0 || fcntl(fd, F_SETFL, flags | O_NONBLOCK) < 0) return -1;
	time_t deadline = monotonic_s() + 2;
	size_t sent = 0;
	while (sent < len) {
		if (monotonic_s() >= deadline) { errno = ETIMEDOUT; break; }
		ssize_t n = write(fd, buf + sent, len - sent);
		if (n > 0) { sent += (size_t)n; continue; }
		if (n < 0 && errno == EINTR) continue;
		if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
			struct pollfd p = { .fd = fd, .events = POLLOUT };
			int rc = poll(&p, 1, 100);
			if (rc < 0 && errno != EINTR) break;
			if (rc > 0 && (p.revents & (POLLERR | POLLHUP | POLLNVAL))) {
				errno = EIO; break;
			}
			continue;
		}
		if (n == 0) errno = EIO;
		break;
	}
	int saved_errno = errno;
	if (fcntl(fd, F_SETFL, flags) < 0) return -1;
	errno = saved_errno;
	return sent == len ? 0 : -1;
}

/* Returns -1 on a transport failure so the producer reopens with backoff.
 * NAK/timeouts leave capture running, retrying only while UBX output is absent. */
static int ubx_config_tick(struct ubx_config *c, int fd, time_t now) {
	if (!c->port) return 0;
	if (c->awaiting_ack && now >= c->ack_deadline) {
		c->awaiting_ack = 0;
		log_msg("UBX RAM setup: no ACK received; continuing capture");
	}
	if (now < c->next_attempt) return 0;
	unsigned char packet[32];
	size_t len = ubx_config_packet(packet, c->port);
	if (!len) { errno = EINVAL; return -1; }
	if (ubx_config_write(fd, packet, len) != 0) {
		log_msg("UBX RAM setup write failed: %s", strerror(errno));
		return -1;
	}
	c->next_attempt = now + UBX_CONFIG_RETRY_S;
	c->ack_deadline = now + UBX_CONFIG_ACK_S;
	c->awaiting_ack = 1;
	log_msg("UBX RAM setup requested on %s: SFRBX/NAV-SAT/MON-RF; baud and NMEA preserved",
		c->port == 1 ? "uart1" : c->port == 2 ? "uart2" : "usb");
	return 0;
}

/* Called only after the stream parser has verified the UBX checksum. */
static void ubx_config_ack(struct ubx_config *c, unsigned cls, unsigned id,
		const unsigned char *payload, unsigned len) {
	if (!c->awaiting_ack || cls != 0x05 || id > 0x01 || len != 2 ||
		payload[0] != 0x06 || payload[1] != 0x8a) return;
	c->awaiting_ack = 0;
	log_msg(id ? "UBX RAM setup acknowledged" :
		"UBX RAM setup rejected; receiver must support the requested CFG-VALSET keys; continuing capture");
}

static void ubx_config_observed(struct ubx_config *c, time_t now) {
	/* A reset behind a still-connected bridge can restore NMEA-only output.
	 * Reapply after 30 s without recognized UBX, as well as on every open. */
	if (c->port) c->next_attempt = now + UBX_CONFIG_RETRY_S;
}
#endif
