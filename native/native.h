/*
 * Copyright 2021 ByteDance Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#ifndef NATIVE_H
#define NATIVE_H

#include <stdint.h>
#include <sys/types.h>
#include <immintrin.h>

#define V_EOF           1
#define V_NULL          2
#define V_TRUE          3
#define V_FALSE         4
#define V_ARRAY         5
#define V_OBJECT        6
#define V_STRING        7
#define V_DOUBLE        8
#define V_INTEGER       9

#define F_DBLUNQ        (1 << 0)
#define F_UNIREP        (1 << 1)

#define VS_NULL         0x6c6c756e      // 'null' in little endian
#define VS_TRUE         0x65757274      // 'true' in little endian
#define VS_ALSE         0x65736c61      // 'alse' in little endian ('false' without the 'f')

#define ERR_EOF         1
#define ERR_INVAL       2
#define ERR_ESCAPE      3
#define ERR_UNICODE     4
#define ERR_OVERFLOW    5
#define ERR_NUMBER_FMT  6
#define ERR_RECURSE_MAX 7

#define MAX_RECURSE     65536

#define likely(v)       (__builtin_expect((v), 1))
#define unlikely(v)     (__builtin_expect((v), 0))

#define as_m128p(v)     ((__m128i *)(v))
#define as_m128c(v)     ((const __m128i *)(v))
#define as_m256c(v)     ((const __m256i *)(v))
#define as_m128v(v)     (*(const __m128i *)(v))

typedef struct {
    char * buf;
    size_t len;
    size_t cap;
} GoSlice;

typedef struct {
    const char * buf;
    size_t       len;
} GoString;

typedef struct {
    long    t;
    double  d;
    int64_t i;
} JsonNumber;

typedef struct {
    long    vt;
    double  dv;
    int64_t iv;
    int64_t ep;
} JsonState;

typedef struct {
    int sp;
    int vt[MAX_RECURSE];
} StateMachine;

int f64toa(char *out, double val);
int i64toa(char *out, int64_t val);
int u64toa(char *out, uint64_t val);

size_t  lzero(const char *p, size_t n);
size_t  lquote(const GoString *s, size_t p);
size_t  lspace(const char *sp, size_t nb, size_t p);
ssize_t unquote(const char *sp, ssize_t nb, char *dp, ssize_t *ep, uint64_t flags);

ssize_t strchr1(const GoString *s, size_t p, char ch);
ssize_t strchr2(const GoString *s, size_t p, char c0, char c1);

long value(const char *s, size_t n, long p, JsonState *ret);
void vstring(const GoString *src, long *p, JsonState *ret);
void vnumber(const GoString *src, long *p, JsonState *ret);
void vsigned(const GoString *src, long *p, JsonState *ret);
void vunsigned(const GoString *src, long *p, JsonState *ret);

long skip_one(const GoString *src, long *p, StateMachine *m);
long skip_array(const GoString *src, long *p, StateMachine *m);
long skip_object(const GoString *src, long *p, StateMachine *m);

long skip_string(const GoString *src, long *p);
long skip_negative(const GoString *src, long *p);
long skip_positive(const GoString *src, long *p);

#endif