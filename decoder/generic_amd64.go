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

package decoder

import (
    `encoding/json`
    `fmt`
    `reflect`

    `github.com/bytedance/sonic/internal/jit`
    `github.com/bytedance/sonic/internal/native`
    `github.com/bytedance/sonic/internal/native/types`
    `github.com/twitchyliquid64/golang-asm/obj`
)

/** Crucial Registers:
 *
 *      ST(BX)  : ro, decoder stack
 *      DF(R10) : ro, decoder flags
 *      EP(R11) : wo, error pointer
 *      IP(R12) : ro, input pointer
 *      IL(R13) : ro, input length
 *      IC(R14) : rw, input cursor
 *      VP(R15) : ro, value pointer (to an interface{})
 */

const (
    _VD_args   = 64     // 64 bytes for passing arguments to other Go functions
    _VD_saves  = 40     // 40 bytes for saving the registers before CALL instructions
    _VD_locals = 40     // 40 bytes for local variables
)

const (
    _VD_offs = _VD_args + _VD_saves + _VD_locals
    _VD_size = _VD_offs + 8     // 8 bytes for the parent frame pointer
)

var (
    _VAR_ss = _VAR_ss_Vt
    _VAR_df = jit.Ptr(_SP, _VD_args + _VD_saves)
)

var (
    _VAR_ss_Vt = jit.Ptr(_SP, _VD_args + _VD_saves + 8)
    _VAR_ss_Dv = jit.Ptr(_SP, _VD_args + _VD_saves + 16)
    _VAR_ss_Iv = jit.Ptr(_SP, _VD_args + _VD_saves + 24)
    _VAR_ss_Ep = jit.Ptr(_SP, _VD_args + _VD_saves + 32)
)

type _ValueDecoder struct {
    jit.BaseAssembler
}

func (self *_ValueDecoder) build() uintptr {
    self.Init(self.compile)
    return *(*uintptr)(self.Load("decode_value", _VD_size, 0))
}

/** Function Calling Helpers **/

func (self *_ValueDecoder) save(r ...obj.Addr) {
    for i, v := range r {
        if i > _VD_saves / 8 - 1 {
            panic("too many registers to save")
        } else {
            self.Emit("MOVQ", v, jit.Ptr(_SP, _VD_args + int64(i) * 8))
        }
    }
}

func (self *_ValueDecoder) load(r ...obj.Addr) {
    for i, v := range r {
        if i > _VD_saves / 8 - 1 {
            panic("too many registers to load")
        } else {
            self.Emit("MOVQ", jit.Ptr(_SP, _VD_args + int64(i) * 8), v)
        }
    }
}

func (self *_ValueDecoder) call(fn obj.Addr) {
    self.Emit("MOVQ", fn, _AX)  // MOVQ ${fn}, AX
    self.Rjmp("CALL", _AX)      // CALL AX
}

func (self *_ValueDecoder) call_go(fn obj.Addr) {
    self.save(_REG_go...)   // SAVE $REG_go
    self.call(fn)           // CALL ${fn}
    self.load(_REG_go...)   // LOAD $REG_go
}

/** Decoder Assembler **/

const (
    _S_val = iota + 1
    _S_arr
    _S_arr_0
    _S_obj
    _S_obj_x
    _S_obj_delim
)

const (
    _S_omask = (1 << _S_obj) | (1 << _S_obj_x)
    _S_vmask = (1 << _S_val) | (1 << _S_arr_0)
)

const (
    _A_init_len = 1
    _A_init_cap = 16
)

const (
    _ST_Sp = 0
    _ST_Vt = _PtrBytes
    _ST_Vp = _PtrBytes * (types.MAX_RECURSE + 1)
)

var (
    _V_true  = jit.Imm(int64(pbool(true)))
    _V_false = jit.Imm(int64(pbool(false)))
    _F_value = jit.Imm(int64(native.S_value))
)

var (
    _V_max     = jit.Imm(int64(types.V_MAX))
    _E_eof     = jit.Imm(int64(types.ERR_EOF))
    _E_invalid = jit.Imm(int64(types.ERR_INVALID_CHAR))
    _E_recurse = jit.Imm(int64(types.ERR_RECURSE_EXCEED_MAX))
)

var (
    _F_convTslice    = jit.Func(convTslice)
    _F_convTstring   = jit.Func(convTstring)
    _F_invalid_vtype = jit.Func(invalid_vtype)
)

var (
    _T_map     = jit.Type(reflect.TypeOf((map[string]interface{})(nil)))
    _T_bool    = jit.Type(reflect.TypeOf(false))
    _T_int64   = jit.Type(reflect.TypeOf(int64(0)))
    _T_eface   = jit.Type(reflect.TypeOf((*interface{})(nil)).Elem())
    _T_slice   = jit.Type(reflect.TypeOf(([]interface{})(nil)))
    _T_string  = jit.Type(reflect.TypeOf(""))
    _T_number  = jit.Type(reflect.TypeOf(json.Number("")))
    _T_float64 = jit.Type(reflect.TypeOf(float64(0)))
)

var _R_tab = map[int]string {
    '[': "_decode_V_ARRAY",
    '{': "_decode_V_OBJECT",
    ':': "_decode_V_KEY_SEP",
    ',': "_decode_V_ELEM_SEP",
    ']': "_decode_V_ARRAY_END",
    '}': "_decode_V_OBJECT_END",
}

func (self *_ValueDecoder) compile() {
    self.Emit("SUBQ", jit.Imm(_VD_size), _SP)       // SUBQ $_VD_size, SP
    self.Emit("MOVQ", _BP, jit.Ptr(_SP, _VD_offs))  // MOVQ BP, _VD_offs(SP)
    self.Emit("LEAQ", jit.Ptr(_SP, _VD_offs), _BP)  // LEAQ _VD_offs(SP), BP

    /* initialize the state machine */
    self.Emit("XORL", _CX, _CX)                                 // XORL CX, CX
    self.Emit("MOVQ", _DF, _VAR_df)                             // MOVQ DF, df
    self.Emit("ADDQ", jit.Imm(_FsmOffset), _ST)                 // ADDQ _FsmOffset, _ST
    self.Emit("MOVQ", _CX, jit.Ptr(_ST, _ST_Sp))                // MOVQ CX, ST.Sp
    self.Emit("MOVQ", _VP, jit.Ptr(_ST, _ST_Vp))                // MOVQ VP, ST.Vp[0]
    self.Emit("MOVQ", jit.Imm(_S_val), jit.Ptr(_ST, _ST_Vt))    // MOVQ _S_val, ST.Vt[0]
    self.Sjmp("JMP" , "_next")                                  // JMP  _next

    /* set the value from previous round */
    self.Link("_set_value")                                 // _set_value:
    self.Emit("MOVL" , jit.Imm(_S_vmask), _DX)              // MOVL  _S_vmask, DX
    self.Emit("MOVQ" , jit.Ptr(_ST, _ST_Sp), _CX)           // MOVQ  ST.Sp, CX
    self.Emit("MOVQ" , jit.Sib(_ST, _CX, 8, _ST_Vt), _AX)   // MOVQ  ST.Vt[CX], AX
    self.Emit("BTQ"  , _AX, _DX)                            // BTQ   AX, DX
    self.Sjmp("JNC"  , "_vtype_error")                      // JNC   _vtype_error
    self.Emit("XORL" , _AX, _AX)                            // XORL  AX, AX
    self.Emit("SUBQ" , jit.Imm(1), jit.Ptr(_ST, _ST_Sp))    // SUBQ  $1, ST.Sp
    self.Emit("XCHGQ", jit.Sib(_ST, _CX, 8, _ST_Vp), _AX)   // XCHGQ ST.Vp[CX], AX
    self.Emit("MOVQ" , _R8, jit.Ptr(_AX, 0))                // MOVQ  R8, (AX)
    self.Emit("MOVQ" , _R9, jit.Ptr(_AX, 8))                // MOVQ  R9, 8(AX)

    /* check for value stack */
    self.Link("_next")                              // _next:
    self.Emit("MOVQ" , jit.Ptr(_ST, _ST_Sp), _AX)   // MOVQ  ST.Sp, AX
    self.Emit("TESTQ", _AX, _AX)                    // TESTQ AX, AX
    self.Sjmp("JS"   , "_return")                   // JS    _return

    /* fast path: test up to 4 characters manually */
    self.Emit("CMPQ"   , _IC, _IL)                      // CMPQ    IC, IL
    self.Sjmp("JAE"    , "_decode_V_EOF")               // JAE     _decode_V_EOF
    self.Emit("MOVBQZX", jit.Sib(_IP, _IC, 1, 0), _AX)  // MOVBQZX (IP)(IC), AX
    self.Emit("MOVQ"   , jit.Imm(_BM_space), _DX)       // MOVQ    _BM_space, DX
    self.Emit("BTQ"    , _AX, _DX)                      // BTQ     _AX, _DX
    self.Sjmp("JNC"    , "_decode_fast")                // JNC     _decode_fast

    /* 1-space case */
    self.Emit("ADDQ"   , jit.Imm(1), _IC)               // ADDQ    $1, IC
    self.Emit("CMPQ"   , _IC, _IL)                      // CMPQ    IC, IL
    self.Sjmp("JAE"    , "_decode_V_EOF")               // JAE     _decode_V_EOF
    self.Emit("MOVBQZX", jit.Sib(_IP, _IC, 1, 0), _AX)  // MOVBQZX (IP)(IC), AX
    self.Emit("MOVQ"   , jit.Imm(_BM_space), _DX)       // MOVQ    _BM_space, DX
    self.Emit("BTQ"    , _AX, _DX)                      // BTQ     _AX, _DX
    self.Sjmp("JNC"    , "_decode_fast")                // JNC     _decode_fast

    /* 2-space case */
    self.Emit("ADDQ"   , jit.Imm(1), _IC)               // ADDQ    $1, IC
    self.Emit("CMPQ"   , _IC, _IL)                      // CMPQ    IC, IL
    self.Sjmp("JAE"    , "_decode_V_EOF")               // JAE     _decode_V_EOF
    self.Emit("MOVBQZX", jit.Sib(_IP, _IC, 1, 0), _AX)  // MOVBQZX (IP)(IC), AX
    self.Emit("MOVQ"   , jit.Imm(_BM_space), _DX)       // MOVQ    _BM_space, DX
    self.Emit("BTQ"    , _AX, _DX)                      // BTQ     _AX, _DX
    self.Sjmp("JNC"    , "_decode_fast")                // JNC     _decode_fast

    /* 3-space case */
    self.Emit("ADDQ"   , jit.Imm(1), _IC)               // ADDQ    $1, IC
    self.Emit("CMPQ"   , _IC, _IL)                      // CMPQ    IC, IL
    self.Sjmp("JAE"    , "_decode_V_EOF")               // JAE     _decode_V_EOF
    self.Emit("MOVBQZX", jit.Sib(_IP, _IC, 1, 0), _AX)  // MOVBQZX (IP)(IC), AX
    self.Emit("MOVQ"   , jit.Imm(_BM_space), _DX)       // MOVQ    _BM_space, DX
    self.Emit("BTQ"    , _AX, _DX)                      // BTQ     _AX, _DX
    self.Sjmp("JNC"    , "_decode_fast")                // JNC     _decode_fast

    /* 4-space case */
    self.Emit("ADDQ"   , jit.Imm(1), _IC)               // ADDQ    $1, IC
    self.Emit("CMPQ"   , _IC, _IL)                      // CMPQ    IC, IL
    self.Sjmp("JAE"    , "_decode_V_EOF")               // JAE     _decode_V_EOF
    self.Emit("MOVBQZX", jit.Sib(_IP, _IC, 1, 0), _AX)  // MOVBQZX (IP)(IC), AX

    /* fast path: use lookup table to select decoder */
    self.Link("_decode_fast")                           // _decode_fast:
    self.Byte(0x48, 0x8d, 0x3d)                         // LEAQ    ?(PC), DI
    self.Sref("_decode_tab", 4)                         // ....    &_decode_tab
    self.Emit("MOVLQSX", jit.Sib(_DI, _AX, 4, 0), _AX)  // MOVLQSX (DI)(AX*4), AX
    self.Emit("TESTQ"  , _AX, _AX)                      // TESTQ   AX, AX
    self.Sjmp("JZ"     , "_decode_native")              // JZ      _decode_native
    self.Emit("ADDQ"   , jit.Imm(1), _IC)               // ADDQ    $1, IC
    self.Emit("ADDQ"   , _DI, _AX)                      // ADDQ    DI, AX
    self.Rjmp("JMP"    , _AX)                           // JMP     AX

    /* decode with native decoder */
    self.Link("_decode_native")         // _decode_native:
    self.Emit("MOVQ", _IP, _DI)         // MOVQ IP, DI
    self.Emit("MOVQ", _IL, _SI)         // MOVQ IL, SI
    self.Emit("MOVQ", _IC, _DX)         // MOVQ IC, DX
    self.Emit("LEAQ", _VAR_ss, _CX)     // LEAQ ss, CX
    self.Emit("MOVL", jit.Imm(1), _R8)  // MOVL $1, R8
    self.call(_F_value)                 // CALL value
    self.Emit("MOVQ", _AX, _IC)         // MOVQ AX, IC

    /* check for errors */
    self.Emit("MOVQ" , _VAR_ss_Vt, _AX)     // MOVQ  ss.Vt, AX
    self.Emit("TESTQ", _AX, _AX)            // TESTQ AX, AX
    self.Sjmp("JS"   , "_parsing_error")    // JS    _parsing_error
    self.Sjmp("JZ"   , "_invalid_vtype")    // JZ    _invalid_vtype
    self.Emit("CMPQ" , _AX, _V_max)         // CMPQ  AX, _V_max
    self.Sjmp("JA"   , "_invalid_vtype")    // JA    _invalid_vtype

    /* jump table selector */
    self.Byte(0x48, 0x8d, 0x3d)                             // LEAQ    ?(PC), DI
    self.Sref("_switch_table", 4)                           // ....    &_switch_table
    self.Emit("MOVLQSX", jit.Sib(_DI, _AX, 4, -4), _AX)     // MOVLQSX -4(DI)(AX*4), AX
    self.Emit("ADDQ"   , _DI, _AX)                          // ADDQ    DI, AX
    self.Rjmp("JMP"    , _AX)                               // JMP     AX

    /** V_EOF **/
    self.Link("_decode_V_EOF")          // _decode_V_EOF:
    self.Emit("MOVL", _E_eof, _EP)      // MOVL _E_eof, EP
    self.Sjmp("JMP" , "_error")         // JMP  _error

    /** V_NULL **/
    self.Link("_decode_V_NULL")                 // _decode_V_NULL:
    self.Emit("XORL", _R8, _R8)                 // XORL R8, R8
    self.Emit("XORL", _R9, _R9)                 // XORL R9, R9
    self.Emit("LEAQ", jit.Ptr(_IC, -4), _DI)    // LEAQ -4(IC), DI
    self.Sjmp("JMP" , "_set_value")             // JMP  _set_value

    /** V_TRUE **/
    self.Link("_decode_V_TRUE")                 // _decode_V_TRUE:
    self.Emit("MOVQ", _T_bool, _R8)             // MOVQ _T_bool, R8
    self.Emit("MOVQ", _V_true, _R9)             // MOVQ _V_true, R9
    self.Emit("LEAQ", jit.Ptr(_IC, -4), _DI)    // LEAQ -4(IC), DI
    self.Sjmp("JMP" , "_set_value")             // JMP  _set_value

    /** V_FALSE **/
    self.Link("_decode_V_FALSE")                // _decode_V_FALSE:
    self.Emit("MOVQ", _T_bool, _R8)             // MOVQ _T_bool, R8
    self.Emit("MOVQ", _V_false, _R9)            // MOVQ _V_false, R9
    self.Emit("LEAQ", jit.Ptr(_IC, -5), _DI)    // LEAQ -5(IC), DI
    self.Sjmp("JMP" , "_set_value")             // JMP  _set_value

    /** V_ARRAY **/
    self.Link("_decode_V_ARRAY")                            // _decode_V_ARRAY:
    self.Emit("MOVL", jit.Imm(_S_vmask), _DX)               // MOVL _S_vmask, DX
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)            // MOVQ ST.Sp, CX
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vt), _AX)    // MOVQ ST.Vt[CX], AX
    self.Emit("BTQ" , _AX, _DX)                             // BTQ  AX, DX
    self.Sjmp("JNC" , "_invalid_char")                      // JNC  _invalid_char

    /* create a new array */
    self.Emit("MOVQ", _T_eface, _AX)                            // MOVQ    _T_eface, AX
    self.Emit("MOVQ", _AX, jit.Ptr(_SP, 0))                     // MOVQ    AX, (SP)
    self.Emit("MOVQ", jit.Imm(_A_init_len), jit.Ptr(_SP, 8))    // MOVQ    _A_init_len, 8(SP)
    self.Emit("MOVQ", jit.Imm(_A_init_cap), jit.Ptr(_SP, 16))   // MOVQ    _A_init_cap, 16(SP)
    self.call_go(_F_makeslice)                                  // CALL_GO runtime.makeslice
    self.Emit("MOVQ", jit.Ptr(_SP, 24), _DX)                    // MOVQ    24(SP), DX

    /* pack into an interface */
    self.Emit("MOVQ", _DX, jit.Ptr(_SP, 0))                     // MOVQ    DX, (SP)
    self.Emit("MOVQ", jit.Imm(_A_init_len), jit.Ptr(_SP, 8))    // MOVQ    _A_init_len, 8(SP)
    self.Emit("MOVQ", jit.Imm(_A_init_cap), jit.Ptr(_SP, 16))   // MOVQ    _A_init_cap, 16(SP)
    self.call_go(_F_convTslice)                                 // CALL_GO runtime.convTslice
    self.Emit("MOVQ", jit.Ptr(_SP, 24), _R8)                    // MOVQ    24(SP), R8

    /* replace current state with an array */
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)                        // MOVQ ST.Sp, CX
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vp), _SI)                // MOVQ ST.Vp[CX], SI
    self.Emit("MOVQ", jit.Imm(_S_arr), jit.Sib(_ST, _CX, 8, _ST_Vt))    // MOVQ _S_arr, ST.Vt[CX]
    self.Emit("MOVQ", _T_slice, _AX)                                    // MOVQ _T_slice, AX
    self.Emit("MOVQ", _AX, jit.Ptr(_SI, 0))                             // MOVQ AX, (SI)
    self.Emit("MOVQ", _R8, jit.Ptr(_SI, 8))                             // MOVQ R8, 8(SI)

    /* add a new slot for the first element */
    self.Emit("ADDQ", jit.Imm(1), _CX)                                  // ADDQ $1, CX
    self.Emit("CMPQ", _CX, jit.Imm(types.MAX_RECURSE))                  // CMPQ CX, ${types.MAX_RECURSE}
    self.Sjmp("JA"  , "_stack_overflow")                                // JA   _stack_overflow
    self.Emit("MOVQ", jit.Ptr(_R8, 0), _AX)                             // MOVQ (R8), AX
    self.Emit("MOVQ", _CX, jit.Ptr(_ST, _ST_Sp))                        // MOVQ CX, ST.Sp
    self.Emit("MOVQ", _AX, jit.Sib(_ST, _CX, 8, _ST_Vp))                // MOVQ AX, ST.Vp[CX]
    self.Emit("MOVQ", jit.Imm(_S_arr_0), jit.Sib(_ST, _CX, 8, _ST_Vt))  // MOVQ _S_arr_0, ST.Vt[CX]
    self.Sjmp("JMP" , "_next")                                          // JMP  _next

    /** V_OBJECT **/
    self.Link("_decode_V_OBJECT")                                       // _decode_V_OBJECT:
    self.Emit("MOVL", jit.Imm(_S_vmask), _DX)                           // MOVL    _S_vmask, DX
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)                        // MOVQ    ST.Sp, CX
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vt), _AX)                // MOVQ    ST.Vt[CX], AX
    self.Emit("BTQ" , _AX, _DX)                                         // BTQ     AX, DX
    self.Sjmp("JNC" , "_invalid_char")                                  // JNC     _invalid_char
    self.call_go(_F_makemap_small)                                      // CALL_GO runtime.makemap_small
    self.Emit("MOVQ", jit.Ptr(_SP, 0), _AX)                             // MOVQ    (SP), AX
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)                        // MOVQ    ST.Sp, CX
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vp), _SI)                // MOVQ    ST.Vp[CX], SI
    self.Emit("MOVQ", _T_map, _DX)                                      // MOVQ    _T_map, DX
    self.Emit("MOVQ", _DX, jit.Ptr(_SI, 0))                             // MOVQ    DX, (SI)
    self.Emit("MOVQ", _AX, jit.Ptr(_SI, 8))                             // MOVQ    AX, 8(SI)
    self.Emit("MOVQ", jit.Imm(_S_obj), jit.Sib(_ST, _CX, 8, _ST_Vt))    // MOVQ    _S_obj, ST.Vt[CX]
    self.Sjmp("JMP" , "_next")                                          // JMP     _next

    /** V_STRING **/
    self.Link("_decode_V_STRING")       // _decode_V_STRING:
    self.Emit("XORL", _DX, _DX)         // XORL DX, DX
    self.Emit("MOVQ", _VAR_ss_Iv, _CX)  // MOVQ ss.Iv, CX
    self.Emit("MOVQ", _IC, _AX)         // MOVQ IC, AX
    self.Emit("SUBQ", _CX, _AX)         // SUBQ CX, AX

    /* check for escapes */
    self.Emit("CMPQ", _VAR_ss_Ep, jit.Imm(-1))          // CMPQ ss.Ep, $-1
    self.Sjmp("JNE" , "_unquote")                       // JNE  _unquote
    self.Emit("SUBQ", jit.Imm(1), _AX)                  // SUBQ $1, AX
    self.Emit("LEAQ", jit.Sib(_IP, _CX, 1, 0), _R8)     // LEAQ (IP)(CX), R8

    /* strings with no escape sequences */
    self.Link("_noescape")                                  // _noescape:
    self.Emit("MOVL", jit.Imm(_S_omask), _DI)               // MOVL _S_omask, DI
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)            // MOVQ ST.Sp, CX
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vt), _SI)    // MOVQ ST.Vt[CX], SI
    self.Emit("BTQ" , _SI, _DI)                             // BTQ  SI, DI
    self.Sjmp("JC"  , "_object_key")                        // JC   _object_key

    /* check for pre-packed strings, avoid 1 allocation */
    self.Emit("TESTQ", _DX, _DX)                // TESTQ   DX, DX
    self.Sjmp("JNZ"  , "_packed_str")           // JNZ     _packed_str
    self.Emit("MOVQ" , _R8, jit.Ptr(_SP, 0))    // MOVQ    R8, (SP)
    self.Emit("MOVQ" , _AX, jit.Ptr(_SP, 8))    // MOVQ    AX, 8(SP)
    self.call_go(_F_convTstring)                // CALL_GO runtime.convTstring
    self.Emit("MOVQ" , jit.Ptr(_SP, 16), _R9)   // MOVQ    16(SP), R9

    /* packed string already in R9 */
    self.Link("_packed_str")            // _packed_str:
    self.Emit("MOVQ", _T_string, _R8)   // MOVQ _T_string, R8
    self.Emit("MOVQ", _VAR_ss_Iv, _DI)  // MOVQ ss.Iv, DI
    self.Emit("SUBQ", jit.Imm(1), _DI)  // SUBQ $1, DI
    self.Sjmp("JMP" , "_set_value")     // JMP  _set_value

    /* the string is an object key, get the map */
    self.Link("_object_key")
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)            // MOVQ ST.Sp, CX
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vp), _SI)    // MOVQ ST.Vp[CX], SI
    self.Emit("MOVQ", jit.Ptr(_SI, 8), _SI)                 // MOVQ 8(SI), SI

    /* add a new delimiter */
    self.Emit("ADDQ", jit.Imm(1), _CX)                                      // ADDQ $1, CX
    self.Emit("CMPQ", _CX, jit.Imm(types.MAX_RECURSE))                      // CMPQ CX, ${types.MAX_RECURSE}
    self.Sjmp("JA"  , "_stack_overflow")                                    // JA   _stack_overflow
    self.Emit("MOVQ", _CX, jit.Ptr(_ST, _ST_Sp))                            // MOVQ CX, ST.Sp
    self.Emit("MOVQ", jit.Imm(_S_obj_delim), jit.Sib(_ST, _CX, 8, _ST_Vt))  // MOVQ _S_obj_delim, ST.Vt[CX]

    /* add a new slot int the map */
    self.Emit("MOVQ", _T_map, _DX)                      // MOVQ    _T_map, DX
    self.Emit("MOVQ", _DX, jit.Ptr(_SP, 0))             // MOVQ    DX, (SP)
    self.Emit("MOVQ", _SI, jit.Ptr(_SP, 8))             // MOVQ    SI, 8(SP)
    self.Emit("MOVQ", _R8, jit.Ptr(_SP, 16))            // MOVQ    R9, 16(SP)
    self.Emit("MOVQ", _AX, jit.Ptr(_SP, 24))            // MOVQ    AX, 24(SP)
    self.call_go(_F_mapassign_faststr)                  // CALL_GO runtime.mapassign_faststr
    self.Emit("MOVQ", jit.Ptr(_SP, 32), _AX)            // MOVQ    32(SP), AX

    /* add to the pointer stack */
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)            // MOVQ ST.Sp, CX
    self.Emit("MOVQ", _AX, jit.Sib(_ST, _CX, 8, _ST_Vp))    // MOVQ AX, ST.Vp[CX]
    self.Sjmp("JMP" , "_next")                              // JMP  _next

    /* allocate memory to store the string header and unquoted result */
    self.Link("_unquote")                               // _unquote:
    self.Emit("ADDQ", jit.Imm(15), _AX)                 // ADDQ    $15, AX
    self.Emit("MOVQ", _T_byte, _CX)                     // MOVQ    _T_byte, CX
    self.Emit("MOVQ", _AX, jit.Ptr(_SP, 0))             // MOVQ    AX, (SP)
    self.Emit("MOVQ", _CX, jit.Ptr(_SP, 8))             // MOVQ    CX, 8(SP)
    self.Emit("MOVB", jit.Imm(0), jit.Ptr(_SP, 16))     // MOVB    $0, 16(SP)
    self.call_go(_F_mallocgc)                           // CALL_GO runtime.mallocgc
    self.Emit("MOVQ", jit.Ptr(_SP, 24), _R9)            // MOVQ    24(SP), R9

    /* prepare the unquoting parameters */
    self.Emit("MOVQ" , _VAR_ss_Iv, _CX)                         // MOVQ  ss.Iv, CX
    self.Emit("LEAQ" , jit.Sib(_IP, _CX, 1, 0), _DI)            // LEAQ  (IP)(CX), DI
    self.Emit("NEGQ" , _CX)                                     // NEGQ  CX
    self.Emit("LEAQ" , jit.Sib(_IC, _CX, 1, -1), _SI)           // LEAQ  -1(IC)(CX), SI
    self.Emit("LEAQ" , jit.Ptr(_R9, 16), _DX)                   // LEAQ  16(R8), DX
    self.Emit("LEAQ" , _VAR_ss_Ep, _CX)                         // LEAQ  ss.Ep, CX
    self.Emit("XORL" , _R8, _R8)                                // XORL  R8, R8
    self.Emit("BTQ"  , jit.Imm(_F_disable_urc), _VAR_df)        // BTQ   ${_F_disable_urc}, fv
    self.Emit("SETCC", _R8)                                     // SETCC R8
    self.Emit("SHLQ" , jit.Imm(types.B_UNICODE_REPLACE), _R8)   // SHLQ  ${types.B_UNICODE_REPLACE}, R8

    /* unquote the string, with R9 been preserved */
    self.save(_R9)                                      // SAVE R9
    self.call(_F_unquote)                               // CALL unquote
    self.load(_R9)                                      // LOAD R9

    /* check for errors */
    self.Emit("TESTQ", _AX, _AX)                // TESTQ AX, AX
    self.Sjmp("JS"   , "_unquote_error")        // JS    _unquote_error
    self.Emit("MOVL" , jit.Imm(1), _DX)         // MOVL  $1, DX
    self.Emit("LEAQ" , jit.Ptr(_R9, 16), _R8)   // ADDQ  $16, R8
    self.Emit("MOVQ" , _R8, jit.Ptr(_R9, 0))    // MOVQ  R8, (R9)
    self.Emit("MOVQ" , _AX, jit.Ptr(_R9, 8))    // MOVQ  AX, 8(R9)
    self.Sjmp("JMP"  , "_noescape")             // JMP   _noescape

    /** V_DOUBLE **/
    self.Link("_decode_V_DOUBLE")                           // _decode_V_DOUBLE:
    self.Emit("BTQ"  , jit.Imm(_F_use_number), _VAR_df)     // BTQ     _F_use_number, df
    self.Sjmp("JC"   , "_use_number")                       // JC      _use_number
    self.Emit("MOVSD", _VAR_ss_Dv, _X0)                     // MOVSD   ss.Dv, X0
    self.Sjmp("JMP"  , "_use_float64")                      // JMP     _use_float64

    /** V_INTEGER **/
    self.Link("_decode_V_INTEGER")                          // _decode_V_INTEGER:
    self.Emit("BTQ"     , jit.Imm(_F_use_number), _VAR_df)  // BTQ      _F_use_number, df
    self.Sjmp("JC"      , "_use_number")                    // JC       _use_number
    self.Emit("BTQ"     , jit.Imm(_F_use_int64), _VAR_df)   // BTQ      _F_use_int64, df
    self.Sjmp("JC"      , "_use_int64")                     // JC       _use_int64
    self.Emit("MOVQ"    , _VAR_ss_Iv, _AX)                  // MOVQ     ss.Iv, AX
    self.Emit("CVTSQ2SD", _AX, _X0)                         // CVTSQ2SD AX, X0

    /* represent number as `float64` */
    self.Link("_use_float64")                   // _use_float64:
    self.Emit("MOVSD", _X0, jit.Ptr(_SP, 0))    // MOVSD   X0, (SP)
    self.call_go(_F_convT64)                    // CALL_GO runtime.convT64
    self.Emit("MOVQ" , _T_float64, _R8)         // MOVQ    _T_float64, R8
    self.Emit("MOVQ" , jit.Ptr(_SP, 8), _R9)    // MOVQ    8(SP), R9
    self.Emit("MOVQ" , _VAR_ss_Ep, _DI)         // MOVQ    ss.Ep, DI
    self.Sjmp("JMP"  , "_set_value")            // JMP     _set_value

    /* represent number as `json.Number` */
    self.Link("_use_number")                            // _use_number
    self.Emit("MOVQ", _VAR_ss_Ep, _AX)                  // MOVQ    ss.Ep, AX
    self.Emit("LEAQ", jit.Sib(_IP, _AX, 1, 0), _SI)     // LEAQ    (IP)(AX), SI
    self.Emit("MOVQ", _IC, _CX)                         // MOVQ    IC, CX
    self.Emit("SUBQ", _AX, _CX)                         // SUBQ    AX, CX
    self.Emit("MOVQ", _SI, jit.Ptr(_SP, 0))             // MOVQ    SI, (SP)
    self.Emit("MOVQ", _CX, jit.Ptr(_SP, 8))             // MOVQ    CX, 8(SP)
    self.call_go(_F_convTstring)                        // CALL_GO runtime.convTstring
    self.Emit("MOVQ", _T_number, _R8)                   // MOVQ    _T_number, R8
    self.Emit("MOVQ", jit.Ptr(_SP, 16), _R9)            // MOVQ    16(SP), R9
    self.Emit("MOVQ", _VAR_ss_Ep, _DI)                  // MOVQ    ss.Ep, DI
    self.Sjmp("JMP" , "_set_value")                     // JMP     _set_value

    /* represent number as `int64` */
    self.Link("_use_int64")                     // _use_int64:
    self.Emit("MOVQ", _VAR_ss_Iv, _AX)          // MOVQ    ss.Iv, AX
    self.Emit("MOVQ", _AX, jit.Ptr(_SP, 0))     // MOVQ    AX, (SP)
    self.call_go(_F_convT64)                    // CALL_GO runtime.convT64
    self.Emit("MOVQ", _T_int64, _R8)            // MOVQ    _T_int64, R8
    self.Emit("MOVQ", jit.Ptr(_SP, 8), _R9)     // MOVQ    8(SP), R9
    self.Emit("MOVQ", _VAR_ss_Ep, _DI)          // MOVQ    ss.Ep, DI
    self.Sjmp("JMP" , "_set_value")             // JMP     _set_value

    /** V_KEY_SEP **/
    self.Link("_decode_V_KEY_SEP")                                          // _decode_V_KEY_SEP:
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)                            // MOVQ ST.Sp, CX
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vt), _AX)                    // MOVQ ST.Vt[CX], AX
    self.Emit("CMPQ", _AX, jit.Imm(_S_obj_delim))                           // CMPQ AX, _S_obj_delim
    self.Sjmp("JNE" , "_invalid_char")                                      // JNE  _invalid_char
    self.Emit("MOVQ", jit.Imm(_S_val), jit.Sib(_ST, _CX, 8, _ST_Vt))        // MOVQ _S_val, ST.Vt[CX]
    self.Emit("MOVQ", jit.Imm(_S_obj), jit.Sib(_ST, _CX, 8, _ST_Vt - 8))    // MOVQ _S_obj, ST.Vt[CX - 1]
    self.Sjmp("JMP" , "_next")                                              // JMP  _next

    /** V_ELEM_SEP **/
    self.Link("_decode_V_ELEM_SEP")                             // _decode_V_ELEM_SEP:
    self.Emit("MOVQ"    , jit.Imm(_S_obj), _AX)                 // MOVQ     _S_obj, AX
    self.Emit("MOVQ"    , jit.Imm(_S_obj_x), _DX)               // MOVQ     _S_obj_x, DX
    self.Emit("MOVQ"    , jit.Ptr(_ST, _ST_Sp), _CX)            // MOVQ     ST.Sp, CX
    self.Emit("CMPXCHGQ", _DX, jit.Sib(_ST, _CX, 8, _ST_Vt))    // CMPXCHGQ DX, ST.Vt[CX]
    self.Sjmp("JZ"      , "_next")                              // JZ       _next
    self.Emit("CMPQ"    , _AX, jit.Imm(_S_arr))                 // CMPQ     _AX, _S_arr
    self.Sjmp("JNE"     , "_invalid_char")                      // JNE      _invalid_char

    /* arrays */
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vp), _SI)    // MOVQ ST.Vp[CX], SI
    self.Emit("MOVQ", jit.Ptr(_SI, 8), _SI)                 // MOVQ 8(SI), SI
    self.Emit("MOVQ", jit.Ptr(_SI, 8), _DX)                 // MOVQ 8(SI), DX
    self.Emit("CMPQ", _DX, jit.Ptr(_SI, 16))                // CMPQ DX, 16(SI)
    self.Sjmp("JAE" , "_array_more")                        // JAE  _array_more

    /* add a slot for the new element */
    self.Link("_array_append")                                          // _array_append:
    self.Emit("ADDQ", jit.Imm(1), jit.Ptr(_SI, 8))                      // ADDQ $1, 8(SI)
    self.Emit("MOVQ", jit.Ptr(_SI, 0), _SI)                             // MOVQ (SI), SI
    self.Emit("ADDQ", jit.Imm(1), _CX)                                  // ADDQ $1, CX
    self.Emit("SHLQ", jit.Imm(1), _DX)                                  // SHLQ $1, DX
    self.Emit("LEAQ", jit.Sib(_SI, _DX, 8, 0), _SI)                     // LEAQ (SI)(DX*8), SI
    self.Emit("MOVQ", _CX, jit.Ptr(_ST, _ST_Sp))                        // MOVQ CX, ST.Sp
    self.Emit("MOVQ", _SI, jit.Sib(_ST, _CX, 8, _ST_Vp))                // MOVQ SI, ST.Vp[CX]
    self.Emit("MOVQ", jit.Imm(_S_val), jit.Sib(_ST, _CX, 8, _ST_Vt))    // MOVQ _S_val, ST.Vt[CX}
    self.Sjmp("JMP" , "_next")                                          // JMP  _next

    /** V_ARRAY_END **/
    self.Link("_decode_V_ARRAY_END")                        // _decode_V_ARRAY_END:
    self.Emit("XORL", _DX, _DX)                             // XORL DX, DX
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)            // MOVQ ST.Sp, CX
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vt), _AX)    // MOVQ ST.Vt[CX], AX
    self.Emit("CMPQ", _AX, jit.Imm(_S_arr_0))               // CMPQ AX, _S_arr_0
    self.Sjmp("JE"  , "_first_item")                        // JE   _first_item
    self.Emit("CMPQ", _AX, jit.Imm(_S_arr))                 // CMPQ AX, _S_arr
    self.Sjmp("JNE" , "_invalid_char")                      // JNE  _invalid_char
    self.Emit("SUBQ", jit.Imm(1), jit.Ptr(_ST, _ST_Sp))     // SUBQ $1, ST.Sp
    self.Emit("MOVQ", _DX, jit.Sib(_ST, _CX, 8, _ST_Vp))    // MOVQ DX, ST.Vp[CX]
    self.Sjmp("JMP" , "_next")                              // JMP  _next

    /* first element of an array */
    self.Link("_first_item")                                    // _first_item:
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)                // MOVQ ST.Sp, CX
    self.Emit("SUBQ", jit.Imm(2), jit.Ptr(_ST, _ST_Sp))         // SUBQ $2, ST.Sp
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vp - 8), _SI)    // MOVQ ST.Vp[CX - 1], SI
    self.Emit("MOVQ", jit.Ptr(_SI, 8), _SI)                     // MOVQ 8(SI), SI
    self.Emit("MOVQ", _DX, jit.Sib(_ST, _CX, 8, _ST_Vp - 8))    // MOVQ DX, ST.Vp[CX - 1]
    self.Emit("MOVQ", _DX, jit.Sib(_ST, _CX, 8, _ST_Vp))        // MOVQ DX, ST.Vp[CX]
    self.Emit("MOVQ", _DX, jit.Ptr(_SI, 8))                     // MOVQ DX, 8(SI)
    self.Sjmp("JMP" , "_next")                                  // JMP  _next

    /** V_OBJECT_END **/
    self.Link("_decode_V_OBJECT_END")                       // _decode_V_OBJECT_END:
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)            // MOVQ ST.Sp, CX
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vt), _AX)    // MOVQ ST.Vt[CX], AX
    self.Emit("CMPQ", _AX, jit.Imm(_S_obj))                 // CMPQ AX, _S_obj
    self.Sjmp("JNE" , "_invalid_char")                      // JNE  _invalid_char
    self.Emit("XORL", _AX, _AX)                             // XORL AX, AX
    self.Emit("SUBQ", jit.Imm(1), jit.Ptr(_ST, _ST_Sp))     // SUBQ $1, ST.Sp
    self.Emit("MOVQ", _AX, jit.Sib(_ST, _CX, 8, _ST_Vp))    // MOVQ AX, ST.Vp[CX]
    self.Sjmp("JMP" , "_next")                              // JMP  _next

    /* return from decoder */
    self.Link("_return")                            // _return:
    self.Emit("XORL", _EP, _EP)                     // XORL EP, EP
    self.Emit("MOVQ", _EP, jit.Ptr(_ST, _ST_Vp))    // MOVQ EP, ST.Vp[0]
    self.Link("_epilogue")                          // _epilogue:
    self.Emit("SUBQ", jit.Imm(_FsmOffset), _ST)     // SUBQ _FsmOffset, _ST
    self.Emit("MOVQ", jit.Ptr(_SP, _VD_offs), _BP)  // MOVQ _VD_offs(SP), BP
    self.Emit("ADDQ", jit.Imm(_VD_size), _SP)       // ADDQ $_VD_size, SP
    self.Emit("RET")                                // RET

    /* array expand */
    self.Link("_array_more")                    // _array_more:
    self.Emit("MOVQ" , _T_eface, _AX)           // MOVQ    _T_eface, AX
    self.Emit("MOVOU", jit.Ptr(_SI, 0), _X0)    // MOVOU   (SI), X0
    self.Emit("MOVQ" , jit.Ptr(_SI, 16), _DX)   // MOVQ    16(SI), DX
    self.Emit("MOVQ" , _AX, jit.Ptr(_SP, 0))    // MOVQ    AX, (SP)
    self.Emit("MOVOU", _X0, jit.Ptr(_SP, 8))    // MOVOU   X0, 8(SP)
    self.Emit("MOVQ" , _DX, jit.Ptr(_SP, 24))   // MOVQ    DX, 24(SP)
    self.Emit("SHLQ" , jit.Imm(1), _DX)         // SHLQ    $1, DX
    self.Emit("MOVQ" , _DX, jit.Ptr(_SP, 32))   // MOVQ    DX, 32(SP)
    self.call_go(_F_growslice)                  // CALL_GO runtime.growslice
    self.Emit("MOVQ" , jit.Ptr(_SP, 40), _DI)   // MOVOU   40(SP), DI
    self.Emit("MOVQ" , jit.Ptr(_SP, 48), _DX)   // MOVOU   48(SP), DX
    self.Emit("MOVQ" , jit.Ptr(_SP, 56), _AX)   // MOVQ    56(SP), AX

    /* update the slice */
    self.Emit("MOVQ", jit.Ptr(_ST, _ST_Sp), _CX)            // MOVQ ST.Sp, CX
    self.Emit("MOVQ", jit.Sib(_ST, _CX, 8, _ST_Vp), _SI)    // MOVQ ST.Vp[CX], SI
    self.Emit("MOVQ", jit.Ptr(_SI, 8), _SI)                 // MOVQ 8(SI), SI
    self.Emit("MOVQ", _DI, jit.Ptr(_SI, 0))                 // MOVQ DI, (SI)
    self.Emit("MOVQ", _DX, jit.Ptr(_SI, 8))                 // MOVQ DX, 8(SI)
    self.Emit("MOVQ", _AX, jit.Ptr(_SI, 16))                // MOVQ AX, 16(AX)
    self.Sjmp("JMP" , "_array_append")                      // JMP  _array_append

    /* error handlers */
    self.Link("_stack_overflow")
    self.Emit("MOVL" , _E_recurse, _EP)         // MOVQ  _E_recurse, EP
    self.Sjmp("JMP"  , "_error")                // JMP   _error
    self.Link("_vtype_error")                   // _vtype_error:
    self.Emit("MOVQ" , _DI, _IC)                // MOVQ  DI, IC
    self.Emit("MOVL" , _E_invalid, _EP)         // MOVL  _E_invalid, EP
    self.Sjmp("JMP"  , "_error")                // JMP   _error
    self.Link("_invalid_char")                  // _invalid_char:
    self.Emit("SUBQ" , jit.Imm(1), _IC)         // SUBQ  $1, IC
    self.Emit("MOVL" , _E_invalid, _EP)         // MOVL  _E_invalid, EP
    self.Sjmp("JMP"  , "_error")                // JMP   _error
    self.Link("_unquote_error")                 // _unquote_error:
    self.Emit("MOVQ" , _VAR_ss_Iv, _IC)         // MOVQ  ss.Iv, IC
    self.Emit("SUBQ" , jit.Imm(1), _IC)         // SUBQ  $1, IC
    self.Link("_parsing_error")                 // _parsing_error:
    self.Emit("NEGQ" , _AX)                     // NEGQ  AX
    self.Emit("MOVQ" , _AX, _EP)                // MOVQ  AX, EP
    self.Link("_error")                         // _error:
    self.Emit("PXOR" , _X0, _X0)                // PXOR  X0, X0
    self.Emit("MOVOU", _X0, jit.Ptr(_VP, 0))    // MOVOU X0, (VP)
    self.Sjmp("JMP"  , "_epilogue")             // JMP   _epilogue

    /* invalid value type, never returns */
    self.Link("_invalid_vtype")
    self.Emit("MOVQ", _AX, jit.Ptr(_SP, 0))     // MOVQ AX, (SP)
    self.call(_F_invalid_vtype)                 // CALL invalid_type
    self.Emit("UD2")                            // UD2

    /* switch jump table */
    self.Link("_switch_table")              // _switch_table:
    self.Sref("_decode_V_EOF", 0)           // SREF &_decode_V_EOF, $0
    self.Sref("_decode_V_NULL", -4)         // SREF &_decode_V_NULL, $-4
    self.Sref("_decode_V_TRUE", -8)         // SREF &_decode_V_TRUE, $-8
    self.Sref("_decode_V_FALSE", -12)       // SREF &_decode_V_FALSE, $-12
    self.Sref("_decode_V_ARRAY", -16)       // SREF &_decode_V_ARRAY, $-16
    self.Sref("_decode_V_OBJECT", -20)      // SREF &_decode_V_OBJECT, $-20
    self.Sref("_decode_V_STRING", -24)      // SREF &_decode_V_STRING, $-24
    self.Sref("_decode_V_DOUBLE", -28)      // SREF &_decode_V_DOUBLE, $-28
    self.Sref("_decode_V_INTEGER", -32)     // SREF &_decode_V_INTEGER, $-32
    self.Sref("_decode_V_KEY_SEP", -36)     // SREF &_decode_V_KEY_SEP, $-36
    self.Sref("_decode_V_ELEM_SEP", -40)    // SREF &_decode_V_ELEM_SEP, $-40
    self.Sref("_decode_V_ARRAY_END", -44)   // SREF &_decode_V_ARRAY_END, $-44
    self.Sref("_decode_V_OBJECT_END", -48)  // SREF &_decode_V_OBJECT_END, $-48

    /* fast character lookup table */
    self.Link("_decode_tab")        // _decode_tab:
    self.Sref("_decode_V_EOF", 0)   // SREF &_decode_V_EOF, $0

    /* generate rest of the tabs */
    for i := 1; i < 256; i++ {
        if to, ok := _R_tab[i]; ok {
            self.Sref(to, -int64(i) * 4)
        } else {
            self.Byte(0x00, 0x00, 0x00, 0x00)
        }
    }
}

/** Generic Decoder **/

var (
    _subr_decode_value = new(_ValueDecoder).build()
)

//go:nosplit
func invalid_vtype(vt types.ValueType) {
    throw(fmt.Sprintf("invalid value type: %d", vt))
}
