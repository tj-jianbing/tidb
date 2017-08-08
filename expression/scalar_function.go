// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package expression

import (
	"bytes"
	"fmt"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/types"
	"github.com/pingcap/tidb/util/types/json"
)

// ScalarFunction is the function that returns a value.
type ScalarFunction struct {
	FuncName model.CIStr
	// RetType is the type that ScalarFunction returns.
	// TODO: Implement type inference here, now we use ast's return type temporarily.
	RetType  *types.FieldType
	Function builtinFunc
}

// GetArgs gets arguments of function.
func (sf *ScalarFunction) GetArgs() []Expression {
	return sf.Function.getArgs()
}

// GetCtx gets the context of function.
func (sf *ScalarFunction) GetCtx() context.Context {
	return sf.Function.getCtx()
}

// String implements fmt.Stringer interface.
func (sf *ScalarFunction) String() string {
	result := sf.FuncName.L + "("
	for i, arg := range sf.GetArgs() {
		result += arg.String()
		if i+1 != len(sf.GetArgs()) {
			result += ", "
		}
	}
	result += ")"
	return result
}

// MarshalJSON implements json.Marshaler interface.
func (sf *ScalarFunction) MarshalJSON() ([]byte, error) {
	buffer := bytes.NewBufferString(fmt.Sprintf("\"%s\"", sf))
	return buffer.Bytes(), nil
}

// NewFunction creates a new scalar function or constant.
func NewFunction(ctx context.Context, funcName string, retType *types.FieldType, args ...Expression) (Expression, error) {
	if funcName == ast.Cast {
		return NewCastFunc(retType, args[0], ctx), nil
	}
	fc, ok := funcs[funcName]
	if !ok {
		return nil, errFunctionNotExists.GenByArgs(funcName)
	}
	funcArgs := make([]Expression, len(args))
	copy(funcArgs, args)
	f, err := fc.getFunction(funcArgs, ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if retType == nil {
		return nil, errors.Errorf("RetType cannot be nil for ScalarFunction.")
	}
	if builtinRetTp := f.getRetTp(); builtinRetTp.Tp != mysql.TypeUnspecified {
		retType = builtinRetTp
	}
	sf := &ScalarFunction{
		FuncName: model.NewCIStr(funcName),
		RetType:  retType,
		Function: f,
	}
	return FoldConstant(sf), nil
}

// ScalarFuncs2Exprs converts []*ScalarFunction to []Expression.
func ScalarFuncs2Exprs(funcs []*ScalarFunction) []Expression {
	result := make([]Expression, 0, len(funcs))
	for _, col := range funcs {
		result = append(result, col)
	}
	return result
}

// Clone implements Expression interface.
func (sf *ScalarFunction) Clone() Expression {
	newArgs := make([]Expression, 0, len(sf.GetArgs()))
	for _, arg := range sf.GetArgs() {
		newArgs = append(newArgs, arg.Clone())
	}
	switch sf.FuncName.L {
	case ast.Cast:
		newFunc, _ := buildCastFunction(sf.GetArgs()[0], sf.GetType(), sf.GetCtx())
		return newFunc
	case ast.Values:
		v := sf.Function.(*builtinValuesSig)
		return NewValuesFunc(v.offset, sf.GetType(), sf.GetCtx())
	}
	newFunc, _ := NewFunction(sf.GetCtx(), sf.FuncName.L, sf.RetType, newArgs...)
	return newFunc
}

// GetType implements Expression interface.
func (sf *ScalarFunction) GetType() *types.FieldType {
	return sf.RetType
}

// GetTypeClass implements Expression interface.
func (sf *ScalarFunction) GetTypeClass() types.TypeClass {
	return sf.RetType.ToClass()
}

// Equal implements Expression interface.
func (sf *ScalarFunction) Equal(e Expression, ctx context.Context) bool {
	fun, ok := e.(*ScalarFunction)
	if !ok {
		return false
	}
	if sf.FuncName.L != fun.FuncName.L {
		return false
	}
	return sf.Function.equal(fun.Function)
}

// IsCorrelated implements Expression interface.
func (sf *ScalarFunction) IsCorrelated() bool {
	for _, arg := range sf.GetArgs() {
		if arg.IsCorrelated() {
			return true
		}
	}
	return false
}

// Decorrelate implements Expression interface.
func (sf *ScalarFunction) Decorrelate(schema *Schema) Expression {
	for i, arg := range sf.GetArgs() {
		sf.GetArgs()[i] = arg.Decorrelate(schema)
	}
	return sf
}

// Eval implements Expression interface.
func (sf *ScalarFunction) Eval(row []types.Datum) (d types.Datum, err error) {
	if !TurnOnNewExprEval {
		return sf.Function.eval(row)
	}
	sc := sf.GetCtx().GetSessionVars().StmtCtx
	var (
		res    interface{}
		isNull bool
	)
	tp := sf.GetType()
	switch sf.GetTypeClass() {
	case types.ClassInt:
		var intRes int64
		intRes, isNull, err = sf.EvalInt(row, sc)
		if mysql.HasUnsignedFlag(tp.Flag) {
			res = uint64(intRes)
		} else {
			res = intRes
		}
	case types.ClassReal:
		res, isNull, err = sf.EvalReal(row, sc)
	case types.ClassDecimal:
		res, isNull, err = sf.EvalDecimal(row, sc)
	case types.ClassString:
		switch x := sf.GetType().Tp; x {
		case mysql.TypeDatetime, mysql.TypeDate, mysql.TypeTimestamp, mysql.TypeNewDate:
			res, isNull, err = sf.EvalTime(row, sc)
		case mysql.TypeDuration:
			res, isNull, err = sf.EvalDuration(row, sc)
		default:
			res, isNull, err = sf.EvalString(row, sc)
		}
	}

	if isNull || err != nil {
		d.SetValue(nil)
		return d, errors.Trace(err)
	}
	d.SetValue(res)
	return
}

// EvalInt implements Expression interface.
func (sf *ScalarFunction) EvalInt(row []types.Datum, sc *variable.StatementContext) (int64, bool, error) {
	return sf.Function.evalInt(row)
}

// EvalReal implements Expression interface.
func (sf *ScalarFunction) EvalReal(row []types.Datum, sc *variable.StatementContext) (float64, bool, error) {
	return sf.Function.evalReal(row)
}

// EvalDecimal implements Expression interface.
func (sf *ScalarFunction) EvalDecimal(row []types.Datum, sc *variable.StatementContext) (*types.MyDecimal, bool, error) {
	return sf.Function.evalDecimal(row)
}

// EvalString implements Expression interface.
func (sf *ScalarFunction) EvalString(row []types.Datum, sc *variable.StatementContext) (string, bool, error) {
	return sf.Function.evalString(row)
}

// EvalTime implements Expression interface.
func (sf *ScalarFunction) EvalTime(row []types.Datum, sc *variable.StatementContext) (types.Time, bool, error) {
	return sf.Function.evalTime(row)
}

// EvalDuration implements Expression interface.
func (sf *ScalarFunction) EvalDuration(row []types.Datum, sc *variable.StatementContext) (types.Duration, bool, error) {
	return sf.Function.evalDuration(row)
}

// EvalJson implements Expression interface.
func (sf *ScalarFunction) EvalJson(row []types.Datum, sc *variable.StatementContext) (json.JSON, bool, error) {
	return sf.Function.evalJson(row)
}

// HashCode implements Expression interface.
func (sf *ScalarFunction) HashCode() []byte {
	var bytes []byte
	v := make([]types.Datum, 0, len(sf.GetArgs())+1)
	bytes, _ = codec.EncodeValue(bytes, types.NewStringDatum(sf.FuncName.L))
	v = append(v, types.NewBytesDatum(bytes))
	for _, arg := range sf.GetArgs() {
		v = append(v, types.NewBytesDatum(arg.HashCode()))
	}
	bytes = bytes[:0]
	bytes, _ = codec.EncodeValue(bytes, v...)
	return bytes
}

// ResolveIndices implements Expression interface.
func (sf *ScalarFunction) ResolveIndices(schema *Schema) {
	for _, arg := range sf.GetArgs() {
		arg.ResolveIndices(schema)
	}
}
