/* Query processor. */
package db

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/HouzuoGuo/tiedot/tdlog"
)

var (
	ErrorMissingLookUp     = Error{"Missing lookup path `in`.", ""}
	ErrorExpectingLookup   = Error{"Expecting vector lookup path `in`, but %v given.", ""}
	ErrorExpectingNumber   = Error{"Expecting `limit` as a number, but %v given.", ""}
	ErrorNeedIndex         = Error{"Please index %v and retry query %v.", ""}
	ErrorExpectingPath     = Error{"Expecting vector path, but %v given.", ""}
	ErrorExpectingPathIn   = Error{"Expecting vector path `in`, but %v given.", ""}
	ErrorExpectingSubQuery = Error{"Expecting a vector of sub-queries, but %v given.", ""}
	ErrorMissingPath       = Error{"Mising path `in`", ""}
	ErrorExpectingInt      = Error{"Expecting `%s` as an integer, but %v given.", ""}
	ErrorMissing           = Error{"Missing `%s`", ""}
	ErrorInvalidPKID       = Error{"%s is not a document PK ID.", ""}
	ErrorNoOP              = Error{"Query %v does not contain any operation (lookup/union/etc)", ""}
)

// Calculate union of sub-query results.
func EvalUnion(exprs []interface{}, src *Col, result *map[int]struct{}) (err error) {
	for _, subExpr := range exprs {
		if err = evalQuery(subExpr, src, result, false); err != nil {
			return
		}
	}
	return
}

// Put all document IDs into result.
func EvalAllIDs(src *Col, result *map[int]struct{}) (err error) {
	src.forEachDoc(func(id int, _ []byte) bool {
		(*result)[id] = struct{}{}
		return true
	}, false)
	return
}

// Value equity check ("attribute == value") using hash lookup.
func Lookup(lookupValue interface{}, expr map[string]interface{}, src *Col, result *map[int]struct{}) (err error) {
	// Figure out lookup path - JSON array "in"
	path, hasPath := expr["in"]
	if !hasPath {
		return ErrorMissingLookUp
	}
	vecPath := make([]string, 0)
	if vecPathInterface, ok := path.([]interface{}); ok {
		for _, v := range vecPathInterface {
			vecPath = append(vecPath, fmt.Sprint(v))
		}
	} else {
		return ErrorExpectingLookup.Fault(path)
	}
	// Figure out result number limit
	intLimit := int(0)
	if limit, hasLimit := expr["limit"]; hasLimit {
		if floatLimit, ok := limit.(float64); ok {
			intLimit = int(floatLimit)
		} else {
			return ErrorExpectingNumber.Fault(limit)
		}
	}
	lookupStrValue := fmt.Sprint(lookupValue) // the value to look for
	lookupValueHash := StrHash(lookupStrValue)
	scanPath := strings.Join(vecPath, INDEX_PATH_SEP)
	if _, indexed := src.indexPaths[scanPath]; !indexed {
		return ErrorNeedIndex.Fault(scanPath, expr)
	}
	num := lookupValueHash % src.db.numParts
	ht := src.hts[num][scanPath]
	ht.Lock.RLock()
	vals := ht.Get(lookupValueHash, intLimit)
	ht.Lock.RUnlock()
	for _, match := range vals {
		// Filter result to avoid hash collision
		if doc, err := src.read(match, false); err == nil {
			for _, v := range GetIn(doc, vecPath) {
				if fmt.Sprint(v) == lookupStrValue {
					(*result)[match] = struct{}{}
				}
			}
		}
	}
	return
}

// Value existence check (value != nil) using hash lookup.
func PathExistence(hasPath interface{}, expr map[string]interface{}, src *Col, result *map[int]struct{}) (err error) {
	// Figure out the path
	vecPath := make([]string, 0)
	if vecPathInterface, ok := hasPath.([]interface{}); ok {
		for _, v := range vecPathInterface {
			vecPath = append(vecPath, fmt.Sprint(v))
		}
	} else {
		return ErrorExpectingPath.Fault(hasPath)

	}
	// Figure out result number limit
	intLimit := 0
	if limit, hasLimit := expr["limit"]; hasLimit {
		if floatLimit, ok := limit.(float64); ok {
			intLimit = int(floatLimit)
		} else {
			return ErrorExpectingNumber.Fault(limit)
		}
	}
	jointPath := strings.Join(vecPath, INDEX_PATH_SEP)
	if _, indexed := src.indexPaths[jointPath]; !indexed {
		return ErrorNeedIndex.Fault(vecPath, expr)
	}
	counter := 0
	partDiv := src.approxDocCount(false) / src.db.numParts / 4000 // collect approx. 4k document IDs in each iteration
	if partDiv == 0 {
		partDiv++
	}
	for iteratePart := 0; iteratePart < src.db.numParts; iteratePart++ {
		ht := src.hts[iteratePart][jointPath]
		ht.Lock.RLock()
		for i := 0; i < partDiv; i++ {
			_, ids := ht.GetPartition(i, partDiv)
			for _, id := range ids {
				(*result)[id] = struct{}{}
				counter++
				if counter == intLimit {
					ht.Lock.RUnlock()
					return nil
				}
			}
		}
		ht.Lock.RUnlock()
	}
	return nil
}

// Calculate intersection of sub-query results.
func Intersect(subExprs interface{}, src *Col, result *map[int]struct{}) (err error) {
	if subExprVecs, ok := subExprs.([]interface{}); ok {
		first := true
		for _, subExpr := range subExprVecs {
			subResult := make(map[int]struct{})
			intersection := make(map[int]struct{})
			if err = evalQuery(subExpr, src, &subResult, false); err != nil {
				return
			}
			if first {
				*result = subResult
				first = false
			} else {
				for k, _ := range subResult {
					if _, inBoth := (*result)[k]; inBoth {
						intersection[k] = struct{}{}
					}
				}
				*result = intersection
			}
		}
	} else {
		return ErrorExpectingSubQuery.Fault(subExprs)
	}
	return
}

// Calculate complement of sub-query results.
func Complement(subExprs interface{}, src *Col, result *map[int]struct{}) (err error) {
	if subExprVecs, ok := subExprs.([]interface{}); ok {
		for _, subExpr := range subExprVecs {
			subResult := make(map[int]struct{})
			complement := make(map[int]struct{})
			if err = evalQuery(subExpr, src, &subResult, false); err != nil {
				return
			}
			for k, _ := range subResult {
				if _, inBoth := (*result)[k]; !inBoth {
					complement[k] = struct{}{}
				}
			}
			for k, _ := range *result {
				if _, inBoth := subResult[k]; !inBoth {
					complement[k] = struct{}{}
				}
			}
			*result = complement
		}
	} else {
		return ErrorExpectingSubQuery.Fault(subExprs)
	}
	return
}

func (col *Col) hashScan(idxName string, key, limit int) []int {
	ht := col.hts[key%col.db.numParts][idxName]
	ht.Lock.RLock()
	vals := ht.Get(key, limit)
	ht.Lock.RUnlock()
	return vals
}

// Look for indexed integer values within the specified integer range.
func IntRange(intFrom interface{}, expr map[string]interface{}, src *Col, result *map[int]struct{}) (err error) {
	path, hasPath := expr["in"]
	if !hasPath {
		return ErrorMissingPath
	}
	// Figure out the path
	vecPath := make([]string, 0)
	if vecPathInterface, ok := path.([]interface{}); ok {
		for _, v := range vecPathInterface {
			vecPath = append(vecPath, fmt.Sprint(v))
		}
	} else {
		return ErrorExpectingPathIn.Fault(path)
	}
	// Figure out result number limit
	intLimit := int(0)
	if limit, hasLimit := expr["limit"]; hasLimit {
		if floatLimit, ok := limit.(float64); ok {
			intLimit = int(floatLimit)
		} else {
			return ErrorExpectingNumber.Fault(limit)
		}
	}
	// Figure out the range ("from" value & "to" value)
	from, to := int(0), int(0)
	if floatFrom, ok := intFrom.(float64); ok {
		from = int(floatFrom)
	} else {
		return ErrorExpectingInt.Fault("int-from", from)
	}
	if intTo, ok := expr["int-to"]; ok {
		if floatTo, ok := intTo.(float64); ok {
			to = int(floatTo)
		} else {
			return ErrorExpectingInt.Fault("int-to", to)
		}
	} else if intTo, ok := expr["int to"]; ok {
		if floatTo, ok := intTo.(float64); ok {
			to = int(floatTo)
		} else {
			return ErrorExpectingInt.Fault("int-to", to)
		}
	} else {
		return ErrorMissing.Fault("int-to")
	}
	if to > from && to-from > 1000 || from > to && from-to > 1000 {
		tdlog.CritNoRepeat("Query %v involves index lookup on more than 1000 values, which can be very inefficient", expr)
	}
	counter := int(0) // Number of results already collected
	htPath := strings.Join(vecPath, ",")
	if _, indexScan := src.indexPaths[htPath]; !indexScan {
		return ErrorNeedIndex.Fault(vecPath, expr)
	}
	if from < to {
		// Forward scan - from low value to high value
		for lookupValue := from; lookupValue <= to; lookupValue++ {
			lookupStrValue := fmt.Sprint(lookupValue)
			hashValue := StrHash(lookupStrValue)
			vals := src.hashScan(htPath, hashValue, int(intLimit))
			for _, docID := range vals {
				if intLimit > 0 && counter == intLimit {
					break
				}
				counter += 1
				(*result)[docID] = struct{}{}
			}
		}
	} else {
		// Backward scan - from high value to low value
		for lookupValue := from; lookupValue >= to; lookupValue-- {
			lookupStrValue := fmt.Sprint(lookupValue)
			hashValue := StrHash(lookupStrValue)
			vals := src.hashScan(htPath, hashValue, int(intLimit))
			for _, docID := range vals {
				if intLimit > 0 && counter == intLimit {
					break
				}
				counter += 1
				(*result)[docID] = struct{}{}
			}
		}
	}
	return
}

func evalQuery(q interface{}, src *Col, result *map[int]struct{}, placeSchemaLock bool) (err error) {
	if placeSchemaLock {
		src.db.schemaLock.RLock()
		defer src.db.schemaLock.RUnlock()
	}
	switch expr := q.(type) {
	case []interface{}: // [sub query 1, sub query 2, etc]
		return EvalUnion(expr, src, result)
	case string:
		if expr == "all" {
			return EvalAllIDs(src, result)
		} else {
			// Might be single document number
			docID, err := strconv.ParseInt(expr, 10, 64)
			if err != nil {
				return ErrorInvalidPKID.Fault(expr)
			}
			(*result)[int(docID)] = struct{}{}
		}
	case map[string]interface{}:
		if lookupValue, lookup := expr["eq"]; lookup { // eq - lookup
			return Lookup(lookupValue, expr, src, result)
		} else if hasPath, exist := expr["has"]; exist { // has - path existence test
			return PathExistence(hasPath, expr, src, result)
		} else if subExprs, intersect := expr["n"]; intersect { // n - intersection
			return Intersect(subExprs, src, result)
		} else if subExprs, complement := expr["c"]; complement { // c - complement
			return Complement(subExprs, src, result)
		} else if intFrom, htRange := expr["int-from"]; htRange { // int-from, int-to - integer range query
			return IntRange(intFrom, expr, src, result)
		} else if intFrom, htRange := expr["int from"]; htRange { // "int from, "int to" - integer range query - same as above, just without dash
			return IntRange(intFrom, expr, src, result)
		} else {
			return ErrorNoOP.Fault(expr)
		}
	}
	return nil
}

// Main entrance to query processor - evaluate a query and put result into result map (as map keys).
func EvalQuery(q interface{}, src *Col, result *map[int]struct{}) (err error) {
	return evalQuery(q, src, result, true)
}

// TODO: How to bring back regex matcher?
// TODO: How to bring back JSON parameterized query?
