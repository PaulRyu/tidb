// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package expression

import (
	"fmt"
	"math"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/mathutil"
	"github.com/pingcap/tipb/go-tipb"
)

var (
	_ functionClass = &arithmeticPlusFunctionClass{}
	_ functionClass = &arithmeticMinusFunctionClass{}
	_ functionClass = &arithmeticDivideFunctionClass{}
	_ functionClass = &arithmeticMultiplyFunctionClass{}
	_ functionClass = &arithmeticIntDivideFunctionClass{}
	_ functionClass = &arithmeticModFunctionClass{}
)

var (
	_ builtinFunc = &builtinArithmeticPlusRealSig{}
	_ builtinFunc = &builtinArithmeticPlusDecimalSig{}
	_ builtinFunc = &builtinArithmeticPlusIntSig{}
	_ builtinFunc = &builtinArithmeticMinusRealSig{}
	_ builtinFunc = &builtinArithmeticMinusDecimalSig{}
	_ builtinFunc = &builtinArithmeticMinusIntSig{}
	_ builtinFunc = &builtinArithmeticDivideRealSig{}
	_ builtinFunc = &builtinArithmeticDivideDecimalSig{}
	_ builtinFunc = &builtinArithmeticMultiplyRealSig{}
	_ builtinFunc = &builtinArithmeticMultiplyDecimalSig{}
	_ builtinFunc = &builtinArithmeticMultiplyIntUnsignedSig{}
	_ builtinFunc = &builtinArithmeticMultiplyIntSig{}
	_ builtinFunc = &builtinArithmeticIntDivideIntSig{}
	_ builtinFunc = &builtinArithmeticIntDivideDecimalSig{}
	_ builtinFunc = &builtinArithmeticModIntUnsignedUnsignedSig{}
	_ builtinFunc = &builtinArithmeticModIntUnsignedSignedSig{}
	_ builtinFunc = &builtinArithmeticModIntSignedUnsignedSig{}
	_ builtinFunc = &builtinArithmeticModIntSignedSignedSig{}
	_ builtinFunc = &builtinArithmeticModRealSig{}
	_ builtinFunc = &builtinArithmeticModDecimalSig{}

	_ builtinFunc = &builtinArithmeticPlusVectorFloat32Sig{}
	_ builtinFunc = &builtinArithmeticMinusVectorFloat32Sig{}
	_ builtinFunc = &builtinArithmeticMultiplyVectorFloat32Sig{}
)

// isConstantBinaryLiteral return true if expr is constant binary literal
func isConstantBinaryLiteral(ctx EvalContext, expr Expression) bool {
	if types.IsBinaryStr(expr.GetType(ctx)) {
		if v, ok := expr.(*Constant); ok {
			if k := v.Value.Kind(); k == types.KindBinaryLiteral {
				return true
			}
		}
	}
	return false
}

// numericContextResultType returns types.EvalType for numeric function's parameters.
// the returned types.EvalType should be one of: types.ETInt, types.ETDecimal, types.ETReal
func numericContextResultType(ctx EvalContext, expr Expression) types.EvalType {
	ft := expr.GetType(ctx)
	if types.IsTypeTemporal(ft.GetType()) {
		if ft.GetDecimal() > 0 {
			return types.ETDecimal
		}
		return types.ETInt
	}
	// to solve https://github.com/pingcap/tidb/issues/27698
	// if expression is constant binary literal, like `0x1234`, `0b00011`, cast to integer
	// for other binary str column related expression, like varbinary, cast to float/double.
	if isConstantBinaryLiteral(ctx, expr) || ft.GetType() == mysql.TypeBit {
		return types.ETInt
	}
	evalTp4Ft := types.ETReal
	if !ft.Hybrid() {
		evalTp4Ft = ft.EvalType()
		if evalTp4Ft != types.ETDecimal && evalTp4Ft != types.ETInt {
			evalTp4Ft = types.ETReal
		}
	}
	return evalTp4Ft
}

// setFlenDecimal4RealOrDecimal is called to set proper `flen` and `decimal` of return
// type according to the two input parameter's types.
func setFlenDecimal4RealOrDecimal(ctx EvalContext, retTp *types.FieldType, arg0, arg1 Expression, isReal, isMultiply bool) {
	a, b := arg0.GetType(ctx), arg1.GetType(ctx)
	if a.GetDecimal() != types.UnspecifiedLength && b.GetDecimal() != types.UnspecifiedLength {
		retTp.SetDecimalUnderLimit(a.GetDecimal() + b.GetDecimal())
		if !isMultiply {
			retTp.SetDecimalUnderLimit(max(a.GetDecimal(), b.GetDecimal()))
		}
		if !isReal && retTp.GetDecimal() > mysql.MaxDecimalScale {
			retTp.SetDecimal(mysql.MaxDecimalScale)
		}
		if a.GetFlen() == types.UnspecifiedLength || b.GetFlen() == types.UnspecifiedLength {
			retTp.SetFlen(types.UnspecifiedLength)
			return
		}
		if isMultiply {
			digitsInt := a.GetFlen() - a.GetDecimal() + b.GetFlen() - b.GetDecimal()
			retTp.SetFlenUnderLimit(digitsInt + retTp.GetDecimal())
		} else {
			digitsInt := max(a.GetFlen()-a.GetDecimal(), b.GetFlen()-b.GetDecimal())
			retTp.SetFlenUnderLimit(digitsInt + retTp.GetDecimal() + 1)
		}
		if isReal {
			retTp.SetFlen(min(retTp.GetFlen(), mysql.MaxRealWidth))
			return
		}
		retTp.SetFlenUnderLimit(min(retTp.GetFlen(), mysql.MaxDecimalWidth))
		return
	}
	if isReal {
		retTp.SetFlen(types.UnspecifiedLength)
		retTp.SetDecimal(types.UnspecifiedLength)
	} else {
		retTp.SetFlen(mysql.MaxDecimalWidth)
		retTp.SetDecimal(mysql.MaxDecimalScale)
	}
}

func (c *arithmeticDivideFunctionClass) setType4DivDecimal(retTp, a, b *types.FieldType, divPrecIncrement int) {
	var deca, decb = a.GetDecimal(), b.GetDecimal()
	if deca == types.UnspecifiedFsp {
		deca = 0
	}
	if decb == types.UnspecifiedFsp {
		decb = 0
	}
	retTp.SetDecimalUnderLimit(deca + divPrecIncrement)
	if a.GetFlen() == types.UnspecifiedLength {
		retTp.SetFlen(mysql.MaxDecimalWidth)
		return
	}
	aPrec := types.DecimalLength2Precision(a.GetFlen(), a.GetDecimal(), mysql.HasUnsignedFlag(a.GetFlag()))
	retTp.SetFlenUnderLimit(aPrec + decb + divPrecIncrement)
	retTp.SetFlenUnderLimit(types.Precision2LengthNoTruncation(retTp.GetFlen(), retTp.GetDecimal(), mysql.HasUnsignedFlag(retTp.GetFlag())))
}

func (c *arithmeticDivideFunctionClass) setType4DivReal(retTp *types.FieldType) {
	retTp.SetDecimal(types.UnspecifiedLength)
	retTp.SetFlen(mysql.MaxRealWidth)
}

type arithmeticPlusFunctionClass struct {
	baseFunctionClass
}

func (c *arithmeticPlusFunctionClass) getFunction(ctx BuildContext, args []Expression) (builtinFunc, error) {
	if err := c.verifyArgs(args); err != nil {
		return nil, err
	}
	if args[0].GetType(ctx.GetEvalCtx()).EvalType().IsVectorKind() || args[1].GetType(ctx.GetEvalCtx()).EvalType().IsVectorKind() {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETVectorFloat32, types.ETVectorFloat32, types.ETVectorFloat32)
		if err != nil {
			return nil, err
		}
		sig := &builtinArithmeticPlusVectorFloat32Sig{bf}
		// sig.setPbCode(tipb.ScalarFuncSig_PlusVectorFloat32)
		return sig, nil
	}
	lhsEvalTp, rhsEvalTp := numericContextResultType(ctx.GetEvalCtx(), args[0]), numericContextResultType(ctx.GetEvalCtx(), args[1])
	if lhsEvalTp == types.ETReal || rhsEvalTp == types.ETReal {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETReal, types.ETReal, types.ETReal)
		if err != nil {
			return nil, err
		}
		setFlenDecimal4RealOrDecimal(ctx.GetEvalCtx(), bf.tp, args[0], args[1], true, false)
		sig := &builtinArithmeticPlusRealSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_PlusReal)
		return sig, nil
	} else if lhsEvalTp == types.ETDecimal || rhsEvalTp == types.ETDecimal {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETDecimal, types.ETDecimal, types.ETDecimal)
		if err != nil {
			return nil, err
		}
		setFlenDecimal4RealOrDecimal(ctx.GetEvalCtx(), bf.tp, args[0], args[1], false, false)
		sig := &builtinArithmeticPlusDecimalSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_PlusDecimal)
		return sig, nil
	}
	bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETInt, types.ETInt, types.ETInt)
	if err != nil {
		return nil, err
	}
	if mysql.HasUnsignedFlag(args[0].GetType(ctx.GetEvalCtx()).GetFlag()) || mysql.HasUnsignedFlag(args[1].GetType(ctx.GetEvalCtx()).GetFlag()) {
		bf.tp.AddFlag(mysql.UnsignedFlag)
	}
	sig := &builtinArithmeticPlusIntSig{bf}
	sig.setPbCode(tipb.ScalarFuncSig_PlusInt)
	return sig, nil
}

type builtinArithmeticPlusIntSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticPlusIntSig) Clone() builtinFunc {
	newSig := &builtinArithmeticPlusIntSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticPlusIntSig) evalInt(ctx EvalContext, row chunk.Row) (val int64, isNull bool, err error) {
	a, isNull, err := s.args[0].EvalInt(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}

	b, isNull, err := s.args[1].EvalInt(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}

	isLHSUnsigned := mysql.HasUnsignedFlag(s.args[0].GetType(ctx).GetFlag())
	isRHSUnsigned := mysql.HasUnsignedFlag(s.args[1].GetType(ctx).GetFlag())

	switch {
	case isLHSUnsigned && isRHSUnsigned:
		if uint64(a) > math.MaxUint64-uint64(b) {
			return 0, true, types.ErrOverflow.GenWithStackByArgs("BIGINT UNSIGNED", fmt.Sprintf("(%s + %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
	case isLHSUnsigned && !isRHSUnsigned:
		if b < 0 && uint64(-b) > uint64(a) {
			return 0, true, types.ErrOverflow.GenWithStackByArgs("BIGINT UNSIGNED", fmt.Sprintf("(%s + %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
		if b > 0 && uint64(a) > math.MaxUint64-uint64(b) {
			return 0, true, types.ErrOverflow.GenWithStackByArgs("BIGINT UNSIGNED", fmt.Sprintf("(%s + %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
	case !isLHSUnsigned && isRHSUnsigned:
		if a < 0 && uint64(-a) > uint64(b) {
			return 0, true, types.ErrOverflow.GenWithStackByArgs("BIGINT UNSIGNED", fmt.Sprintf("(%s + %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
		if a > 0 && uint64(b) > math.MaxUint64-uint64(a) {
			return 0, true, types.ErrOverflow.GenWithStackByArgs("BIGINT UNSIGNED", fmt.Sprintf("(%s + %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
	case !isLHSUnsigned && !isRHSUnsigned:
		if (a > 0 && b > math.MaxInt64-a) || (a < 0 && b < math.MinInt64-a) {
			return 0, true, types.ErrOverflow.GenWithStackByArgs("BIGINT", fmt.Sprintf("(%s + %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
	}

	return a + b, false, nil
}

type builtinArithmeticPlusDecimalSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticPlusDecimalSig) Clone() builtinFunc {
	newSig := &builtinArithmeticPlusDecimalSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticPlusDecimalSig) evalDecimal(ctx EvalContext, row chunk.Row) (*types.MyDecimal, bool, error) {
	a, isNull, err := s.args[0].EvalDecimal(ctx, row)
	if isNull || err != nil {
		return nil, isNull, err
	}
	b, isNull, err := s.args[1].EvalDecimal(ctx, row)
	if isNull || err != nil {
		return nil, isNull, err
	}
	c := &types.MyDecimal{}
	err = types.DecimalAdd(a, b, c)
	if err != nil {
		if err == types.ErrOverflow {
			err = types.ErrOverflow.GenWithStackByArgs("DECIMAL", fmt.Sprintf("(%s + %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
		return nil, true, err
	}
	return c, false, nil
}

type builtinArithmeticPlusRealSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticPlusRealSig) Clone() builtinFunc {
	newSig := &builtinArithmeticPlusRealSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticPlusRealSig) evalReal(ctx EvalContext, row chunk.Row) (float64, bool, error) {
	a, isLHSNull, err := s.args[0].EvalReal(ctx, row)
	if err != nil {
		return 0, isLHSNull, err
	}
	b, isRHSNull, err := s.args[1].EvalReal(ctx, row)
	if err != nil {
		return 0, isRHSNull, err
	}
	if isLHSNull || isRHSNull {
		return 0, true, nil
	}
	if !mathutil.IsFinite(a + b) {
		return 0, true, types.ErrOverflow.GenWithStackByArgs("DOUBLE", fmt.Sprintf("(%s + %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
	}
	return a + b, false, nil
}

type arithmeticMinusFunctionClass struct {
	baseFunctionClass
}

func (c *arithmeticMinusFunctionClass) getFunction(ctx BuildContext, args []Expression) (builtinFunc, error) {
	if err := c.verifyArgs(args); err != nil {
		return nil, err
	}
	if args[0].GetType(ctx.GetEvalCtx()).EvalType().IsVectorKind() || args[1].GetType(ctx.GetEvalCtx()).EvalType().IsVectorKind() {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETVectorFloat32, types.ETVectorFloat32, types.ETVectorFloat32)
		if err != nil {
			return nil, err
		}
		sig := &builtinArithmeticMinusVectorFloat32Sig{bf}
		// sig.setPbCode(tipb.ScalarFuncSig_PlusVectorFloat32)
		return sig, nil
	}
	lhsEvalTp, rhsEvalTp := numericContextResultType(ctx.GetEvalCtx(), args[0]), numericContextResultType(ctx.GetEvalCtx(), args[1])
	if lhsEvalTp == types.ETReal || rhsEvalTp == types.ETReal {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETReal, types.ETReal, types.ETReal)
		if err != nil {
			return nil, err
		}
		setFlenDecimal4RealOrDecimal(ctx.GetEvalCtx(), bf.tp, args[0], args[1], true, false)
		sig := &builtinArithmeticMinusRealSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_MinusReal)
		return sig, nil
	} else if lhsEvalTp == types.ETDecimal || rhsEvalTp == types.ETDecimal {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETDecimal, types.ETDecimal, types.ETDecimal)
		if err != nil {
			return nil, err
		}
		setFlenDecimal4RealOrDecimal(ctx.GetEvalCtx(), bf.tp, args[0], args[1], false, false)
		sig := &builtinArithmeticMinusDecimalSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_MinusDecimal)
		return sig, nil
	}
	bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETInt, types.ETInt, types.ETInt)
	if err != nil {
		return nil, err
	}
	if (mysql.HasUnsignedFlag(args[0].GetType(ctx.GetEvalCtx()).GetFlag()) || mysql.HasUnsignedFlag(args[1].GetType(ctx.GetEvalCtx()).GetFlag())) && !ctx.GetEvalCtx().SQLMode().HasNoUnsignedSubtractionMode() {
		bf.tp.AddFlag(mysql.UnsignedFlag)
	}
	sig := &builtinArithmeticMinusIntSig{baseBuiltinFunc: bf}
	sig.setPbCode(tipb.ScalarFuncSig_MinusInt)
	return sig, nil
}

type builtinArithmeticMinusRealSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticMinusRealSig) Clone() builtinFunc {
	newSig := &builtinArithmeticMinusRealSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticMinusRealSig) evalReal(ctx EvalContext, row chunk.Row) (float64, bool, error) {
	a, isNull, err := s.args[0].EvalReal(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	b, isNull, err := s.args[1].EvalReal(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	if !mathutil.IsFinite(a - b) {
		return 0, true, types.ErrOverflow.GenWithStackByArgs("DOUBLE", fmt.Sprintf("(%s - %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
	}
	return a - b, false, nil
}

type builtinArithmeticMinusDecimalSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticMinusDecimalSig) Clone() builtinFunc {
	newSig := &builtinArithmeticMinusDecimalSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticMinusDecimalSig) evalDecimal(ctx EvalContext, row chunk.Row) (*types.MyDecimal, bool, error) {
	a, isNull, err := s.args[0].EvalDecimal(ctx, row)
	if isNull || err != nil {
		return nil, isNull, err
	}
	b, isNull, err := s.args[1].EvalDecimal(ctx, row)
	if isNull || err != nil {
		return nil, isNull, err
	}
	c := &types.MyDecimal{}
	err = types.DecimalSub(a, b, c)
	if err != nil {
		if err == types.ErrOverflow {
			err = types.ErrOverflow.GenWithStackByArgs("DECIMAL", fmt.Sprintf("(%s - %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
		return nil, true, err
	}
	return c, false, nil
}

type builtinArithmeticMinusIntSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticMinusIntSig) Clone() builtinFunc {
	newSig := &builtinArithmeticMinusIntSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticMinusIntSig) evalInt(ctx EvalContext, row chunk.Row) (val int64, isNull bool, err error) {
	a, isNull, err := s.args[0].EvalInt(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}

	b, isNull, err := s.args[1].EvalInt(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	forceToSigned := sqlMode(ctx).HasNoUnsignedSubtractionMode()
	isLHSUnsigned := mysql.HasUnsignedFlag(s.args[0].GetType(ctx).GetFlag())
	isRHSUnsigned := mysql.HasUnsignedFlag(s.args[1].GetType(ctx).GetFlag())

	errType := "BIGINT UNSIGNED"
	signed := forceToSigned || (!isLHSUnsigned && !isRHSUnsigned)
	if signed {
		errType = "BIGINT"
	}
	overflow := s.overflowCheck(isLHSUnsigned, isRHSUnsigned, signed, a, b)
	if overflow {
		return 0, true, types.ErrOverflow.GenWithStackByArgs(errType, fmt.Sprintf("(%s - %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
	}

	return a - b, false, nil
}

// returns true when overflowed
func (s *builtinArithmeticMinusIntSig) overflowCheck(isLHSUnsigned, isRHSUnsigned, signed bool, a, b int64) bool {
	res := a - b
	ua, ub := uint64(a), uint64(b)
	resUnsigned := false
	if isLHSUnsigned {
		if isRHSUnsigned {
			if ua < ub {
				if res >= 0 {
					return true
				}
			} else {
				resUnsigned = true
			}
		} else {
			if b >= 0 {
				if ua > ub {
					resUnsigned = true
				}
			} else {
				if testIfSumOverflowsUll(ua, uint64(-b)) {
					return true
				}
				resUnsigned = true
			}
		}
	} else {
		if isRHSUnsigned {
			if uint64(a-math.MinInt64) < ub {
				return true
			}
		} else {
			if a > 0 && b < 0 {
				resUnsigned = true
			} else if a < 0 && b > 0 && res >= 0 {
				return true
			}
		}
	}

	if (!signed && !resUnsigned && res < 0) || (signed && resUnsigned && uint64(res) > uint64(math.MaxInt64)) {
		return true
	}

	return false
}

func testIfSumOverflowsUll(a, b uint64) bool {
	return math.MaxUint64-a < b
}

type arithmeticMultiplyFunctionClass struct {
	baseFunctionClass
}

func (c *arithmeticMultiplyFunctionClass) getFunction(ctx BuildContext, args []Expression) (builtinFunc, error) {
	if err := c.verifyArgs(args); err != nil {
		return nil, err
	}
	if args[0].GetType(ctx.GetEvalCtx()).EvalType().IsVectorKind() || args[1].GetType(ctx.GetEvalCtx()).EvalType().IsVectorKind() {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETVectorFloat32, types.ETVectorFloat32, types.ETVectorFloat32)
		if err != nil {
			return nil, err
		}
		sig := &builtinArithmeticMultiplyVectorFloat32Sig{bf}
		// sig.setPbCode(tipb.ScalarFuncSig_PlusVectorFloat32)
		return sig, nil
	}
	lhsTp, rhsTp := args[0].GetType(ctx.GetEvalCtx()), args[1].GetType(ctx.GetEvalCtx())
	lhsEvalTp, rhsEvalTp := numericContextResultType(ctx.GetEvalCtx(), args[0]), numericContextResultType(ctx.GetEvalCtx(), args[1])
	if lhsEvalTp == types.ETReal || rhsEvalTp == types.ETReal {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETReal, types.ETReal, types.ETReal)
		if err != nil {
			return nil, err
		}
		setFlenDecimal4RealOrDecimal(ctx.GetEvalCtx(), bf.tp, args[0], args[1], true, true)
		sig := &builtinArithmeticMultiplyRealSig{bf, false}
		sig.setPbCode(tipb.ScalarFuncSig_MultiplyReal)
		return sig, nil
	} else if lhsEvalTp == types.ETDecimal || rhsEvalTp == types.ETDecimal {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETDecimal, types.ETDecimal, types.ETDecimal)
		if err != nil {
			return nil, err
		}
		setFlenDecimal4RealOrDecimal(ctx.GetEvalCtx(), bf.tp, args[0], args[1], false, true)
		sig := &builtinArithmeticMultiplyDecimalSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_MultiplyDecimal)
		return sig, nil
	}
	bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETInt, types.ETInt, types.ETInt)
	if err != nil {
		return nil, err
	}
	if mysql.HasUnsignedFlag(lhsTp.GetFlag()) || mysql.HasUnsignedFlag(rhsTp.GetFlag()) {
		bf.tp.AddFlag(mysql.UnsignedFlag)
		sig := &builtinArithmeticMultiplyIntUnsignedSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_MultiplyIntUnsigned)
		return sig, nil
	}
	sig := &builtinArithmeticMultiplyIntSig{bf}
	sig.setPbCode(tipb.ScalarFuncSig_MultiplyInt)
	return sig, nil
}

type builtinArithmeticMultiplyRealSig struct {
	baseBuiltinFunc

	test bool
	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticMultiplyRealSig) Clone() builtinFunc {
	newSig := &builtinArithmeticMultiplyRealSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

type builtinArithmeticMultiplyDecimalSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticMultiplyDecimalSig) Clone() builtinFunc {
	newSig := &builtinArithmeticMultiplyDecimalSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

type builtinArithmeticMultiplyIntUnsignedSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticMultiplyIntUnsignedSig) Clone() builtinFunc {
	newSig := &builtinArithmeticMultiplyIntUnsignedSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

type builtinArithmeticMultiplyIntSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticMultiplyIntSig) Clone() builtinFunc {
	newSig := &builtinArithmeticMultiplyIntSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticMultiplyRealSig) evalReal(ctx EvalContext, row chunk.Row) (float64, bool, error) {
	a, isNull, err := s.args[0].EvalReal(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	b, isNull, err := s.args[1].EvalReal(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	result := a * b
	if math.IsInf(result, 0) {
		return 0, true, types.ErrOverflow.GenWithStackByArgs("DOUBLE", fmt.Sprintf("(%s * %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
	}
	return result, false, nil
}

func (s *builtinArithmeticMultiplyDecimalSig) evalDecimal(ctx EvalContext, row chunk.Row) (*types.MyDecimal, bool, error) {
	a, isNull, err := s.args[0].EvalDecimal(ctx, row)
	if isNull || err != nil {
		return nil, isNull, err
	}
	b, isNull, err := s.args[1].EvalDecimal(ctx, row)
	if isNull || err != nil {
		return nil, isNull, err
	}
	c := &types.MyDecimal{}
	err = types.DecimalMul(a, b, c)
	if err != nil && !terror.ErrorEqual(err, types.ErrTruncated) {
		if err == types.ErrOverflow {
			err = types.ErrOverflow.GenWithStackByArgs("DECIMAL", fmt.Sprintf("(%s * %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
		return nil, true, err
	}
	return c, false, nil
}

func (s *builtinArithmeticMultiplyIntUnsignedSig) evalInt(ctx EvalContext, row chunk.Row) (val int64, isNull bool, err error) {
	a, isNull, err := s.args[0].EvalInt(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	unsignedA := uint64(a)
	b, isNull, err := s.args[1].EvalInt(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	unsignedB := uint64(b)
	result := unsignedA * unsignedB
	if unsignedA != 0 && result/unsignedA != unsignedB {
		return 0, true, types.ErrOverflow.GenWithStackByArgs("BIGINT UNSIGNED", fmt.Sprintf("(%s * %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
	}
	return int64(result), false, nil
}

func (s *builtinArithmeticMultiplyIntSig) evalInt(ctx EvalContext, row chunk.Row) (val int64, isNull bool, err error) {
	a, isNull, err := s.args[0].EvalInt(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	b, isNull, err := s.args[1].EvalInt(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	result := a * b
	if (a != 0 && result/a != b) || (result == math.MinInt64 && a == -1) {
		return 0, true, types.ErrOverflow.GenWithStackByArgs("BIGINT", fmt.Sprintf("(%s * %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
	}
	return result, false, nil
}

type arithmeticDivideFunctionClass struct {
	baseFunctionClass
}

func (c *arithmeticDivideFunctionClass) getFunction(ctx BuildContext, args []Expression) (builtinFunc, error) {
	if err := c.verifyArgs(args); err != nil {
		return nil, err
	}
	lhsEvalTp, rhsEvalTp := numericContextResultType(ctx.GetEvalCtx(), args[0]), numericContextResultType(ctx.GetEvalCtx(), args[1])
	if lhsEvalTp == types.ETReal || rhsEvalTp == types.ETReal {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETReal, types.ETReal, types.ETReal)
		if err != nil {
			return nil, err
		}
		c.setType4DivReal(bf.tp)
		sig := &builtinArithmeticDivideRealSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_DivideReal)
		return sig, nil
	}
	bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETDecimal, types.ETDecimal, types.ETDecimal)
	if err != nil {
		return nil, err
	}
	lhsTp, rhsTp := args[0].GetType(ctx.GetEvalCtx()), args[1].GetType(ctx.GetEvalCtx())
	c.setType4DivDecimal(bf.tp, lhsTp, rhsTp, ctx.GetEvalCtx().GetDivPrecisionIncrement())
	sig := &builtinArithmeticDivideDecimalSig{bf}
	sig.setPbCode(tipb.ScalarFuncSig_DivideDecimal)
	return sig, nil
}

type builtinArithmeticDivideRealSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticDivideRealSig) Clone() builtinFunc {
	newSig := &builtinArithmeticDivideRealSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

type builtinArithmeticDivideDecimalSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticDivideDecimalSig) Clone() builtinFunc {
	newSig := &builtinArithmeticDivideDecimalSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticDivideRealSig) evalReal(ctx EvalContext, row chunk.Row) (float64, bool, error) {
	a, isNull, err := s.args[0].EvalReal(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	b, isNull, err := s.args[1].EvalReal(ctx, row)
	if isNull || err != nil {
		return 0, isNull, err
	}
	if b == 0 {
		return 0, true, handleDivisionByZeroError(ctx)
	}
	result := a / b
	if math.IsInf(result, 0) {
		return 0, true, types.ErrOverflow.GenWithStackByArgs("DOUBLE", fmt.Sprintf("(%s / %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
	}
	return result, false, nil
}

func (s *builtinArithmeticDivideDecimalSig) evalDecimal(ctx EvalContext, row chunk.Row) (*types.MyDecimal, bool, error) {
	a, isNull, err := s.args[0].EvalDecimal(ctx, row)
	if isNull || err != nil {
		return nil, isNull, err
	}

	b, isNull, err := s.args[1].EvalDecimal(ctx, row)
	if isNull || err != nil {
		return nil, isNull, err
	}

	c := &types.MyDecimal{}
	err = types.DecimalDiv(a, b, c, ctx.GetDivPrecisionIncrement())
	if err == types.ErrDivByZero {
		return c, true, handleDivisionByZeroError(ctx)
	} else if err == types.ErrTruncated {
		tc := typeCtx(ctx)
		err = tc.HandleTruncate(errTruncatedWrongValue.GenWithStackByArgs("DECIMAL", c))
	} else if err == nil {
		_, frac := c.PrecisionAndFrac()
		if frac < s.baseBuiltinFunc.tp.GetDecimal() {
			err = c.Round(c, s.baseBuiltinFunc.tp.GetDecimal(), types.ModeHalfUp)
		}
	} else if err == types.ErrOverflow {
		err = types.ErrOverflow.GenWithStackByArgs("DECIMAL", fmt.Sprintf("(%s / %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
	}
	return c, false, err
}

type arithmeticIntDivideFunctionClass struct {
	baseFunctionClass
}

func (c *arithmeticIntDivideFunctionClass) getFunction(ctx BuildContext, args []Expression) (builtinFunc, error) {
	if err := c.verifyArgs(args); err != nil {
		return nil, err
	}
	lhsTp, rhsTp := args[0].GetType(ctx.GetEvalCtx()), args[1].GetType(ctx.GetEvalCtx())
	lhsEvalTp, rhsEvalTp := numericContextResultType(ctx.GetEvalCtx(), args[0]), numericContextResultType(ctx.GetEvalCtx(), args[1])
	if lhsEvalTp == types.ETInt && rhsEvalTp == types.ETInt {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETInt, types.ETInt, types.ETInt)
		if err != nil {
			return nil, err
		}
		if mysql.HasUnsignedFlag(lhsTp.GetFlag()) || mysql.HasUnsignedFlag(rhsTp.GetFlag()) {
			bf.tp.AddFlag(mysql.UnsignedFlag)
		}
		sig := &builtinArithmeticIntDivideIntSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_IntDivideInt)
		return sig, nil
	}
	bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETInt, types.ETDecimal, types.ETDecimal)
	if err != nil {
		return nil, err
	}
	if mysql.HasUnsignedFlag(lhsTp.GetFlag()) || mysql.HasUnsignedFlag(rhsTp.GetFlag()) {
		bf.tp.AddFlag(mysql.UnsignedFlag)
	}
	sig := &builtinArithmeticIntDivideDecimalSig{bf}
	sig.setPbCode(tipb.ScalarFuncSig_IntDivideDecimal)
	return sig, nil
}

type builtinArithmeticIntDivideIntSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticIntDivideIntSig) Clone() builtinFunc {
	newSig := &builtinArithmeticIntDivideIntSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

type builtinArithmeticIntDivideDecimalSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticIntDivideDecimalSig) Clone() builtinFunc {
	newSig := &builtinArithmeticIntDivideDecimalSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticIntDivideIntSig) evalInt(ctx EvalContext, row chunk.Row) (int64, bool, error) {
	a, aIsNull, err := s.args[0].EvalInt(ctx, row)
	if aIsNull || err != nil {
		return 0, aIsNull, err
	}
	b, bIsNull, err := s.args[1].EvalInt(ctx, row)
	if bIsNull || err != nil {
		return 0, bIsNull, err
	}

	if b == 0 {
		return 0, true, handleDivisionByZeroError(ctx)
	}

	var (
		ret int64
		val uint64
	)
	isLHSUnsigned := mysql.HasUnsignedFlag(s.args[0].GetType(ctx).GetFlag())
	isRHSUnsigned := mysql.HasUnsignedFlag(s.args[1].GetType(ctx).GetFlag())

	switch {
	case isLHSUnsigned && isRHSUnsigned:
		ret = int64(uint64(a) / uint64(b))
	case isLHSUnsigned && !isRHSUnsigned:
		val, err = types.DivUintWithInt(uint64(a), b)
		ret = int64(val)
	case !isLHSUnsigned && isRHSUnsigned:
		val, err = types.DivIntWithUint(a, uint64(b))
		ret = int64(val)
	case !isLHSUnsigned && !isRHSUnsigned:
		ret, err = types.DivInt64(a, b)
	}

	return ret, err != nil, err
}

func (s *builtinArithmeticIntDivideDecimalSig) evalInt(ctx EvalContext, row chunk.Row) (ret int64, isNull bool, err error) {
	ec := errCtx(ctx)
	var num [2]*types.MyDecimal
	for i, arg := range s.args {
		num[i], isNull, err = arg.EvalDecimal(ctx, row)
		if isNull || err != nil {
			return 0, isNull, err
		}
	}

	c := &types.MyDecimal{}
	err = types.DecimalDiv(num[0], num[1], c, ctx.GetDivPrecisionIncrement())
	if err == types.ErrDivByZero {
		return 0, true, handleDivisionByZeroError(ctx)
	}
	if err == types.ErrTruncated {
		err = ec.HandleError(errTruncatedWrongValue.GenWithStackByArgs("DECIMAL", c))
	}
	if err == types.ErrOverflow {
		newErr := errTruncatedWrongValue.GenWithStackByArgs("DECIMAL", c)
		err = ec.HandleError(newErr)
	}
	if err != nil {
		return 0, true, err
	}

	isLHSUnsigned := mysql.HasUnsignedFlag(s.args[0].GetType(ctx).GetFlag())
	isRHSUnsigned := mysql.HasUnsignedFlag(s.args[1].GetType(ctx).GetFlag())

	if isLHSUnsigned || isRHSUnsigned {
		val, err := c.ToUint()
		// err returned by ToUint may be ErrTruncated or ErrOverflow, only handle ErrOverflow, ignore ErrTruncated.
		if err == types.ErrOverflow {
			v, err := c.ToInt()
			// when the final result is at (-1, 0], it should be return 0 instead of the error
			if v == 0 && err == types.ErrTruncated {
				ret = int64(0)
				return ret, false, nil
			}
			return 0, true, types.ErrOverflow.GenWithStackByArgs("BIGINT UNSIGNED", fmt.Sprintf("(%s DIV %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
		ret = int64(val)
	} else {
		ret, err = c.ToInt()
		// err returned by ToInt may be ErrTruncated or ErrOverflow, only handle ErrOverflow, ignore ErrTruncated.
		if err == types.ErrOverflow {
			return 0, true, types.ErrOverflow.GenWithStackByArgs("BIGINT", fmt.Sprintf("(%s DIV %s)", s.args[0].StringWithCtx(ctx, errors.RedactLogDisable), s.args[1].StringWithCtx(ctx, errors.RedactLogDisable)))
		}
	}

	return ret, false, nil
}

type arithmeticModFunctionClass struct {
	baseFunctionClass
}

func (c *arithmeticModFunctionClass) setType4ModRealOrDecimal(retTp, a, b *types.FieldType, isDecimal bool) {
	if a.GetDecimal() == types.UnspecifiedLength || b.GetDecimal() == types.UnspecifiedLength {
		retTp.SetDecimal(types.UnspecifiedLength)
	} else {
		retTp.SetDecimalUnderLimit(max(a.GetDecimal(), b.GetDecimal()))
	}

	if a.GetFlen() == types.UnspecifiedLength || b.GetFlen() == types.UnspecifiedLength {
		retTp.SetFlen(types.UnspecifiedLength)
	} else {
		retTp.SetFlen(max(a.GetFlen(), b.GetFlen()))
		if isDecimal {
			retTp.SetFlenUnderLimit(retTp.GetFlen())
			return
		}
		retTp.SetFlen(min(retTp.GetFlen(), mysql.MaxRealWidth))
	}
}

func (c *arithmeticModFunctionClass) getFunction(ctx BuildContext, args []Expression) (builtinFunc, error) {
	if err := c.verifyArgs(args); err != nil {
		return nil, err
	}
	lhsTp, rhsTp := args[0].GetType(ctx.GetEvalCtx()), args[1].GetType(ctx.GetEvalCtx())
	lhsEvalTp, rhsEvalTp := numericContextResultType(ctx.GetEvalCtx(), args[0]), numericContextResultType(ctx.GetEvalCtx(), args[1])
	if lhsEvalTp == types.ETReal || rhsEvalTp == types.ETReal {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETReal, types.ETReal, types.ETReal)
		if err != nil {
			return nil, err
		}
		c.setType4ModRealOrDecimal(bf.tp, lhsTp, rhsTp, false)
		if mysql.HasUnsignedFlag(lhsTp.GetFlag()) {
			bf.tp.AddFlag(mysql.UnsignedFlag)
		}
		sig := &builtinArithmeticModRealSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_ModReal)
		return sig, nil
	} else if lhsEvalTp == types.ETDecimal || rhsEvalTp == types.ETDecimal {
		bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETDecimal, types.ETDecimal, types.ETDecimal)
		if err != nil {
			return nil, err
		}
		c.setType4ModRealOrDecimal(bf.tp, lhsTp, rhsTp, true)
		if mysql.HasUnsignedFlag(lhsTp.GetFlag()) {
			bf.tp.AddFlag(mysql.UnsignedFlag)
		}
		sig := &builtinArithmeticModDecimalSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_ModDecimal)
		return sig, nil
	}
	bf, err := newBaseBuiltinFuncWithTp(ctx, c.funcName, args, types.ETInt, types.ETInt, types.ETInt)
	if err != nil {
		return nil, err
	}
	if mysql.HasUnsignedFlag(lhsTp.GetFlag()) {
		bf.tp.AddFlag(mysql.UnsignedFlag)
	}
	isLHSUnsigned := mysql.HasUnsignedFlag(args[0].GetType(ctx.GetEvalCtx()).GetFlag())
	isRHSUnsigned := mysql.HasUnsignedFlag(args[1].GetType(ctx.GetEvalCtx()).GetFlag())
	switch {
	case isLHSUnsigned && isRHSUnsigned:
		sig := &builtinArithmeticModIntUnsignedUnsignedSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_ModIntUnsignedUnsigned)
		return sig, nil
	case isLHSUnsigned && !isRHSUnsigned:
		sig := &builtinArithmeticModIntUnsignedSignedSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_ModIntUnsignedSigned)
		return sig, nil
	case !isLHSUnsigned && isRHSUnsigned:
		sig := &builtinArithmeticModIntSignedUnsignedSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_ModIntSignedUnsigned)
		return sig, nil
	default:
		sig := &builtinArithmeticModIntSignedSignedSig{bf}
		sig.setPbCode(tipb.ScalarFuncSig_ModIntSignedSigned)
		return sig, nil
	}
}

type builtinArithmeticModRealSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticModRealSig) Clone() builtinFunc {
	newSig := &builtinArithmeticModRealSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticModRealSig) evalReal(ctx EvalContext, row chunk.Row) (float64, bool, error) {
	a, aIsNull, err := s.args[0].EvalReal(ctx, row)
	if err != nil {
		return 0, false, err
	}

	b, bIsNull, err := s.args[1].EvalReal(ctx, row)
	if err != nil {
		return 0, false, err
	}

	if aIsNull || bIsNull {
		return 0, true, nil
	}

	if b == 0 {
		return 0, true, handleDivisionByZeroError(ctx)
	}

	return math.Mod(a, b), false, nil
}

type builtinArithmeticModDecimalSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticModDecimalSig) Clone() builtinFunc {
	newSig := &builtinArithmeticModDecimalSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticModDecimalSig) evalDecimal(ctx EvalContext, row chunk.Row) (*types.MyDecimal, bool, error) {
	a, isNull, err := s.args[0].EvalDecimal(ctx, row)
	if isNull || err != nil {
		return nil, isNull, err
	}
	b, isNull, err := s.args[1].EvalDecimal(ctx, row)
	if isNull || err != nil {
		return nil, isNull, err
	}
	c := &types.MyDecimal{}
	err = types.DecimalMod(a, b, c)
	if err == types.ErrDivByZero {
		return c, true, handleDivisionByZeroError(ctx)
	}
	return c, err != nil, err
}

type builtinArithmeticModIntUnsignedUnsignedSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticModIntUnsignedUnsignedSig) Clone() builtinFunc {
	newSig := &builtinArithmeticModIntUnsignedUnsignedSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticModIntUnsignedUnsignedSig) evalInt(ctx EvalContext, row chunk.Row) (val int64, isNull bool, err error) {
	a, aIsNull, err := s.args[0].EvalInt(ctx, row)
	if err != nil {
		return 0, false, err
	}

	b, bIsNull, err := s.args[1].EvalInt(ctx, row)
	if err != nil {
		return 0, false, err
	}

	if aIsNull || bIsNull {
		return 0, true, nil
	}

	if b == 0 {
		return 0, true, handleDivisionByZeroError(ctx)
	}

	ret := int64(uint64(a) % uint64(b))

	return ret, false, nil
}

type builtinArithmeticModIntUnsignedSignedSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticModIntUnsignedSignedSig) Clone() builtinFunc {
	newSig := &builtinArithmeticModIntUnsignedSignedSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticModIntUnsignedSignedSig) evalInt(ctx EvalContext, row chunk.Row) (val int64, isNull bool, err error) {
	a, aIsNull, err := s.args[0].EvalInt(ctx, row)
	if err != nil {
		return 0, false, err
	}

	b, bIsNull, err := s.args[1].EvalInt(ctx, row)
	if err != nil {
		return 0, false, err
	}

	if aIsNull || bIsNull {
		return 0, true, nil
	}

	if b == 0 {
		return 0, true, handleDivisionByZeroError(ctx)
	}

	var ret int64
	if b < 0 {
		ret = int64(uint64(a) % uint64(-b))
	} else {
		ret = int64(uint64(a) % uint64(b))
	}

	return ret, false, nil
}

type builtinArithmeticModIntSignedUnsignedSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticModIntSignedUnsignedSig) Clone() builtinFunc {
	newSig := &builtinArithmeticModIntSignedUnsignedSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticModIntSignedUnsignedSig) evalInt(ctx EvalContext, row chunk.Row) (val int64, isNull bool, err error) {
	a, aIsNull, err := s.args[0].EvalInt(ctx, row)
	if err != nil {
		return 0, false, err
	}

	b, bIsNull, err := s.args[1].EvalInt(ctx, row)
	if err != nil {
		return 0, false, err
	}

	if aIsNull || bIsNull {
		return 0, true, nil
	}

	if b == 0 {
		return 0, true, handleDivisionByZeroError(ctx)
	}

	var ret int64
	if a < 0 {
		ret = -int64(uint64(-a) % uint64(b))
	} else {
		ret = int64(uint64(a) % uint64(b))
	}

	return ret, false, nil
}

type builtinArithmeticModIntSignedSignedSig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticModIntSignedSignedSig) Clone() builtinFunc {
	newSig := &builtinArithmeticModIntSignedSignedSig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticModIntSignedSignedSig) evalInt(ctx EvalContext, row chunk.Row) (val int64, isNull bool, err error) {
	a, aIsNull, err := s.args[0].EvalInt(ctx, row)
	if err != nil {
		return 0, false, err
	}

	b, bIsNull, err := s.args[1].EvalInt(ctx, row)
	if err != nil {
		return 0, false, err
	}

	if aIsNull || bIsNull {
		return 0, true, nil
	}

	if b == 0 {
		return 0, true, handleDivisionByZeroError(ctx)
	}

	return a % b, false, nil
}

type builtinArithmeticPlusVectorFloat32Sig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticPlusVectorFloat32Sig) Clone() builtinFunc {
	newSig := &builtinArithmeticPlusVectorFloat32Sig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticPlusVectorFloat32Sig) evalVectorFloat32(ctx EvalContext, row chunk.Row) (types.VectorFloat32, bool, error) {
	a, isLHSNull, err := s.args[0].EvalVectorFloat32(ctx, row)
	if err != nil {
		return types.ZeroVectorFloat32, isLHSNull, err
	}
	b, isRHSNull, err := s.args[1].EvalVectorFloat32(ctx, row)
	if err != nil {
		return types.ZeroVectorFloat32, isRHSNull, err
	}
	if isLHSNull || isRHSNull {
		return types.ZeroVectorFloat32, true, nil
	}
	v, err := a.Add(b)
	if err != nil {
		return types.ZeroVectorFloat32, true, err
	}
	return v, false, nil
}

type builtinArithmeticMinusVectorFloat32Sig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticMinusVectorFloat32Sig) Clone() builtinFunc {
	newSig := &builtinArithmeticMinusVectorFloat32Sig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticMinusVectorFloat32Sig) evalVectorFloat32(ctx EvalContext, row chunk.Row) (types.VectorFloat32, bool, error) {
	a, isLHSNull, err := s.args[0].EvalVectorFloat32(ctx, row)
	if err != nil {
		return types.ZeroVectorFloat32, isLHSNull, err
	}
	b, isRHSNull, err := s.args[1].EvalVectorFloat32(ctx, row)
	if err != nil {
		return types.ZeroVectorFloat32, isRHSNull, err
	}
	if isLHSNull || isRHSNull {
		return types.ZeroVectorFloat32, true, nil
	}
	v, err := a.Sub(b)
	if err != nil {
		return types.ZeroVectorFloat32, true, err
	}
	return v, false, nil
}

type builtinArithmeticMultiplyVectorFloat32Sig struct {
	baseBuiltinFunc

	// NOTE: Any new fields added here must be thread-safe or immutable during execution,
	// as this expression may be shared across sessions.
	// If a field does not meet these requirements, set SafeToShareAcrossSession to false.
}

func (s *builtinArithmeticMultiplyVectorFloat32Sig) Clone() builtinFunc {
	newSig := &builtinArithmeticMultiplyVectorFloat32Sig{}
	newSig.cloneFrom(&s.baseBuiltinFunc)
	return newSig
}

func (s *builtinArithmeticMultiplyVectorFloat32Sig) evalVectorFloat32(ctx EvalContext, row chunk.Row) (types.VectorFloat32, bool, error) {
	a, isLHSNull, err := s.args[0].EvalVectorFloat32(ctx, row)
	if err != nil {
		return types.ZeroVectorFloat32, isLHSNull, err
	}
	b, isRHSNull, err := s.args[1].EvalVectorFloat32(ctx, row)
	if err != nil {
		return types.ZeroVectorFloat32, isRHSNull, err
	}
	if isLHSNull || isRHSNull {
		return types.ZeroVectorFloat32, true, nil
	}
	v, err := a.Mul(b)
	if err != nil {
		return types.ZeroVectorFloat32, true, err
	}
	return v, false, nil
}
