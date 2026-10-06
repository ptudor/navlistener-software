/* Exercise the real reader and setup state machine without receiver hardware. */
#define main navfeeder_main
#include "navfeeder.c"
#undef main
#include <assert.h>

static void no_output(int fd) {
	struct pollfd p = { .fd = fd, .events = POLLIN };
	assert(poll(&p, 1, 0) == 0);
}

static void check_valset(const unsigned char packet[32], const uint32_t keys[4]) {
	assert(!memcmp(packet, "\xb5\x62\x06\x8a\x18\x00\x00\x01\x00\x00", 10));
	/* Assert the entire key set: adding a persistent layer, baud or NMEA key
	 * must fail this test, even when the rest of the receiver setup still works. */
	for (unsigned i = 0; i < 4; i++) {
		assert(rd_le32(packet + 10 + i * 5) == keys[i]);
		assert(packet[14 + i * 5] == 1);
	}
	unsigned sum = 0, weighted = 0;
	for (unsigned i = 2; i < 30; i++) { sum += packet[i]; weighted += (30-i)*packet[i]; }
	assert(packet[30] == (sum & 255) && packet[31] == (weighted & 255));
}

/* check_packet reads both VALSETs: the raw-navigation setup first, then the
 * receiver-solution messages, so a receiver rejecting the second keeps the first. */
static void check_packet(int fd, int port) {
	unsigned char packet[32];
	unsigned p = (unsigned)port;
	assert(recv(fd, packet, sizeof packet, MSG_WAITALL) == sizeof packet);
	const uint32_t core[4] = { port == 1 ? 0x10740001u : port == 2 ? 0x10760001u : 0x10780001u,
		0x20910231u + p, 0x20910015u + p, 0x20910359u + p };
	check_valset(packet, core);
	assert(recv(fd, packet, sizeof packet, MSG_WAITALL) == sizeof packet);
	/* NAV-PVT, NAV-STATUS, NAV-CLOCK, NAV-EOE on the same port. */
	const uint32_t solution[4] = { 0x20910006u + p, 0x2091001au + p, 0x20910065u + p, 0x2091015fu + p };
	check_valset(packet, solution);
}

static void test_setup_and_recovery(void) {
	int fd[2]; assert(socketpair(AF_UNIX, SOCK_STREAM, 0, fd) == 0);
	struct ubx_config c = {0};
	assert(ubx_config_tick(&c, fd[0], 100) == 0);
	no_output(fd[1]); /* passive sources never receive configuration */
	for (int port = 1; port <= 3; port++) {
		c = (struct ubx_config){ .port = port };
		int flags = fcntl(fd[0], F_GETFL);
		assert(ubx_config_tick(&c, fd[0], 100) == 0);
		assert((fcntl(fd[0], F_GETFL) & O_NONBLOCK) == (flags & O_NONBLOCK));
		check_packet(fd[1], port);
		assert(c.awaiting_ack == UBX_CONFIG_PACKETS);
		const unsigned char unrelated[] = {0x06, 0x01}, ack[] = {0x06, 0x8a};
		ubx_config_ack(&c, 5, 1, unrelated, 2);
		ubx_config_ack(&c, 5, 1, ack, 1);
		assert(c.awaiting_ack == UBX_CONFIG_PACKETS);
		ubx_config_ack(&c, 5, 1, ack, 2); /* the raw-navigation setup */
		assert(c.awaiting_ack == 1);
		ubx_config_ack(&c, 5, 1, ack, 2); /* the receiver-solution setup */
		assert(!c.awaiting_ack);
		ubx_config_ack(&c, 5, 1, ack, 2); /* nothing outstanding: ignored */
		assert(!c.awaiting_ack);
		/* Healthy output suppresses retries; ACK alone does not. */
		ubx_config_observed(&c, 125);
		assert(ubx_config_tick(&c, fd[0], 130) == 0);
		no_output(fd[1]);
		assert(ubx_config_tick(&c, fd[0], 155) == 0);
		check_packet(fd[1], port);
		/* A NAK of either leaves input capture and a bounded retry available. */
		ubx_config_ack(&c, 5, 1, ack, 2);
		ubx_config_ack(&c, 5, 0, ack, 2);
		assert(!c.awaiting_ack);
		assert(ubx_config_tick(&c, fd[0], 184) == 0);
		no_output(fd[1]);
		assert(ubx_config_tick(&c, fd[0], 185) == 0);
		check_packet(fd[1], port);
		assert(ubx_config_tick(&c, fd[0], 188) == 0);
		assert(!c.awaiting_ack);
		no_output(fd[1]);
	}
	/* A powered-down receiver behind a live UART bridge resumes NMEA only.
	 * Force the recovery deadline due and drive the real buffered sync scan. */
	struct rdbuf rb = { .fd = fd[0], .config = { .port = 1, .next_attempt = 1 } };
	const unsigned char nmea[] = "$GNTXT,01,01,02,boot*00\r\n\xb5\x62";
	assert(write(fd[1], nmea, sizeof nmea - 1) == sizeof nmea - 1);
	assert(sync_ubx(&rb) == 0);
	check_packet(fd[1], 1);
	close(fd[0]); close(fd[1]);
}

static void *receiver(void *arg) {
	int fd = *(int *)arg;
	check_packet(fd, 1);
	/* Interleave ACK and NAV-SAT with NMEA. A setup response must not consume
	 * or flush the navigation message needed by the ordinary spool path. */
	const unsigned char prefix[] = "$GNTXT,01,01,02,test*00\r\n\xb5\x62\x05\x01\x02\x00\x06\x8a\x98\xc1";
	assert(write(fd, prefix, sizeof prefix - 1) == sizeof prefix - 1);
	unsigned char sat[28] = {0xb5,0x62,0x01,0x35,20,0, 0,0,0,0,1,1,0,0,
		0,7,42,30,0,0,0,0,8,0,0,0,0,0};
	for (unsigned i = 2; i < 26; i++) { sat[26] += sat[i]; sat[27] += sat[26]; }
	assert(write(fd, sat, sizeof sat) == sizeof sat);
	assert(shutdown(fd, SHUT_WR) == 0);
	return NULL;
}

static void test_reader_delivery(void) {
	int fd[2]; assert(socketpair(AF_UNIX, SOCK_STREAM, 0, fd) == 0);
	assert(spool_init(&g_spool, 8, NULL, 0) == 0);
	pthread_t peer; assert(pthread_create(&peer, NULL, receiver, &fd[1]) == 0);
	assert(run_ubx(fd[0], 1));
	assert(pthread_join(peer, NULL) == 0);
	assert(g_spool.count == 1);
	struct frame *f = &g_spool.ring[g_spool.head];
	assert(f->data[12] == F_T_RECEPTION);
	assert(f->len == RECORD_HDR + 8);
	assert(f->data[RECORD_HDR + 4] == 7); /* SV7 survived setup/ACK */
	free(f->data);
	free(g_spool.ring);
	pthread_mutex_destroy(&g_spool.mu);
	close(fd[0]); close(fd[1]);
}

static void test_write_failure(void) {
	int fd[2]; assert(socketpair(AF_UNIX, SOCK_STREAM, 0, fd) == 0);
	int flags = fcntl(fd[0], F_GETFL);
	assert(fcntl(fd[0], F_SETFL, flags | O_NONBLOCK) == 0);
	char fill[4096] = {0};
	while (write(fd[0], fill, sizeof fill) > 0) {}
	assert(errno == EAGAIN || errno == EWOULDBLOCK);
	assert(fcntl(fd[0], F_SETFL, flags) == 0);
	unsigned char packet[32]; assert(ubx_config_packet(packet, 1, 0) == sizeof packet);
	assert(ubx_config_packet(packet, 1, UBX_CONFIG_PACKETS) == 0 && ubx_config_packet(packet, 4, 0) == 0);
	time_t start = monotonic_s();
	assert(ubx_config_write(fd[0], packet, sizeof packet) == -1);
	assert(errno == ETIMEDOUT && monotonic_s() - start <= 3);
	assert((fcntl(fd[0], F_GETFL) & O_NONBLOCK) == (flags & O_NONBLOCK));
	close(fd[0]); close(fd[1]);
}

#ifndef GOLDEN
#error GOLDEN must name testdata/receiver_solution_v1.txt
#endif

static size_t golden_hex(const char *name, unsigned char *out, size_t cap) {
	FILE *f = fopen(GOLDEN, "r");
	assert(f);
	char line[512], key[32], hex[400];
	size_t n = 0;
	while (fgets(line, sizeof line, f)) {
		if (line[0] == '#' || sscanf(line, "%31s %399s", key, hex) != 2 || strcmp(key, name)) continue;
		n = strlen(hex) / 2;
		assert(n <= cap);
		for (size_t i = 0; i < n; i++) { unsigned v; assert(sscanf(hex + 2*i, "%2x", &v) == 1); out[i] = (unsigned char)v; }
	}
	fclose(f);
	assert(n);
	return n;
}

static void *solution_receiver(void *arg) {
	int fd = *(int *)arg;
	static const char *const names[] = {"nav-pvt", "nav-clock", "nav-status"};
	static const unsigned char ids[] = {0x07, 0x22, 0x03};
	unsigned char payload[128], tow[4];
	for (unsigned m = 0; m < 3; m++) {
		size_t len = golden_hex(names[m], payload, sizeof payload);
		if (m == 0) memcpy(tow, payload, 4);
		unsigned char msg[136] = {0xb5, 0x62, 0x01, ids[m], (unsigned char)len, 0};
		memcpy(msg + 6, payload, len);
		unsigned char a = 0, b = 0;
		for (size_t i = 2; i < len + 6; i++) { a += msg[i]; b += a; }
		msg[len + 6] = a; msg[len + 7] = b;
		assert(write(fd, msg, len + 8) == (ssize_t)(len + 8));
	}
	unsigned char eoe[12] = {0xb5, 0x62, 0x01, 0x61, 4, 0, tow[0], tow[1], tow[2], tow[3]};
	for (unsigned i = 2; i < 10; i++) { eoe[10] += eoe[i]; eoe[11] += eoe[10]; }
	assert(write(fd, eoe, sizeof eoe) == sizeof eoe);
	assert(shutdown(fd, SHUT_WR) == 0);
	return NULL;
}

/* test_solution_delivery: the golden epoch through the real reader is spooled as one
 * receiver-solution record whose body is the golden body. */
static void test_solution_delivery(void) {
	int fd[2]; assert(socketpair(AF_UNIX, SOCK_STREAM, 0, fd) == 0);
	assert(spool_init(&g_spool, 8, NULL, 0) == 0);
	pthread_t peer; assert(pthread_create(&peer, NULL, solution_receiver, &fd[1]) == 0);
	uint64_t before = now_unix_ns();
	assert(run_ubx(fd[0], 0));
	assert(pthread_join(peer, NULL) == 0);
	assert(g_spool.count == 1);
	struct frame *f = &g_spool.ring[g_spool.head];
	unsigned char body[RS_BODY_MAX];
	size_t n = golden_hex("body", body, sizeof body);
	assert(f->data[12] == RS_TELEM_TYPE && f->len == RECORD_HDR + n);
	assert(!memcmp(f->data + RECORD_HDR, body, n));
	uint64_t stamp = 0;
	for (int i = 0; i < 8; i++) stamp = stamp << 8 | f->data[i];
	assert(stamp >= before && stamp <= now_unix_ns());
	free(f->data);
	free(g_spool.ring);
	pthread_mutex_destroy(&g_spool.mu);
	close(fd[0]); close(fd[1]);
}

int main(void) {
	assert(ubx_config_port("uart1") == 1 && ubx_config_port("uart2") == 2);
	assert(ubx_config_port("usb") == 3 && ubx_config_port("auto") == 0);
	test_setup_and_recovery();
	test_reader_delivery();
	test_solution_delivery();
	test_write_failure();
	puts("UBX RAM setup, NMEA-only recovery, interleaved delivery, golden receiver solution and write timeout: PASS");
	return 0;
}
