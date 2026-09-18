# SPDX-License-Identifier: GPL-2.0
LINUX ?= ../linux
OUTPUT ?= ../.build/kselftest
CC ?= cc
CFLAGS := -std=gnu11 -O2 -Wall -Wextra -Werror
CPPFLAGS += -I$(OUTPUT)/headers/include -I$(LINUX)/tools/testing/selftests
LDLIBS += -pthread

.PHONY: all clean
all: $(OUTPUT)/aok-object-test $(OUTPUT)/aok-task-test $(OUTPUT)/aok-resource-test $(OUTPUT)/aok-eventsrc-test $(OUTPUT)/aok-eventwake-test $(OUTPUT)/aok-core-test

$(OUTPUT)/aok-eventwake-test: eventwake.c $(OUTPUT)/headers/include/linux/aok.h
	mkdir -p $(OUTPUT)
	$(CC) $(CPPFLAGS) $(CFLAGS) -static $< $(LDLIBS) -o $@

$(OUTPUT)/aok-core-test: core.c $(OUTPUT)/headers/include/linux/aok.h
	mkdir -p $(OUTPUT)
	$(CC) $(CPPFLAGS) $(CFLAGS) -static $< $(LDLIBS) -o $@

$(OUTPUT)/aok-resource-test: resource.c $(OUTPUT)/headers/include/linux/aok.h
	mkdir -p $(OUTPUT)
	$(CC) $(CPPFLAGS) $(CFLAGS) -static $< $(LDLIBS) -o $@

$(OUTPUT)/aok-eventsrc-test: eventsrc.c $(OUTPUT)/headers/include/linux/aok.h
	mkdir -p $(OUTPUT)
	$(CC) $(CPPFLAGS) $(CFLAGS) -static $< $(LDLIBS) -o $@

$(OUTPUT)/aok-object-test: object.c $(OUTPUT)/headers/include/linux/aok.h
	mkdir -p $(OUTPUT)
	$(CC) $(CPPFLAGS) $(CFLAGS) -static $< $(LDLIBS) -o $@

$(OUTPUT)/aok-task-test: task.c $(OUTPUT)/headers/include/linux/aok.h
	mkdir -p $(OUTPUT)
	$(CC) $(CPPFLAGS) $(CFLAGS) -static $< $(LDLIBS) -o $@

clean:
	rm -f $(OUTPUT)/aok-object-test $(OUTPUT)/aok-task-test $(OUTPUT)/aok-resource-test $(OUTPUT)/aok-eventsrc-test $(OUTPUT)/aok-eventwake-test $(OUTPUT)/aok-core-test
