#include "textflag.h"

TEXT ·addAsm(SB),NOSPLIT,$0-24
	MOVQ	a+0(FP), AX
	ADDQ	b+8(FP), AX
	MOVQ	AX, ret+16(FP)
	RET

TEXT ·allocAsm(SB),$8-8
	CALL	·newInt(SB)
	MOVQ	0(SP), AX
	MOVQ	AX, ret+0(FP)
	RET
