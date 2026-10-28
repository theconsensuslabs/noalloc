#include "textflag.h"

TEXT ·addAsm(SB),NOSPLIT,$0-24
	MOVD	a+0(FP), R0
	MOVD	b+8(FP), R1
	ADD	R1, R0, R0
	MOVD	R0, ret+16(FP)
	RET

TEXT ·allocAsm(SB),$16-8
	CALL	·newInt(SB)
	MOVD	8(RSP), R0
	MOVD	R0, ret+0(FP)
	RET
