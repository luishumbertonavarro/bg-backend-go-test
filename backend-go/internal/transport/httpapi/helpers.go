package httpapi

import (
	"bytes"
	"errors"
	"net/http"
)

// errTooLarge señala un cuerpo por encima del límite.
var errTooLarge = errors.New("cuerpo demasiado grande")

// requireMethod rechaza cualquier verbo distinto del esperado.
//
// Hace falta porque net/http registra el handler por RUTA, no por método: sin
// esta comprobación, el mismo handler atendería PUT, DELETE o HEAD.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"reason": "METHOD_NOT_ALLOWED"})
	return false
}

// readLimited lee el cuerpo cortando un byte por encima del máximo, igual que
// hace el canal WebSocket con los frames grandes.
//
// Cortar en streaming y no medir después es lo que hace que el límite proteja:
// leer el cuerpo entero para luego rechazarlo permitiría tumbar el proceso
// mandando cuerpos gigantes.
func readLimited(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()

	buf := new(bytes.Buffer)
	n, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, errTooLarge
	}
	return buf.Bytes(), nil
}
