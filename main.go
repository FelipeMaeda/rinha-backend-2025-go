package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type Payment struct {
	ID     string `json:"id"`
	Amount string `json:"amount"`
}

const (
	queueDir       = "/tmp"
	priorityDir    = "/tmp/priority"
	summaryFile    = "/data/summary.json"
	logFilePath    = "/data/app.log"
	mainURL        = "http://main-endpoint:8080/payments"
	fallbackURL    = "http://fallback-endpoint:8080/payments"
	workerCount    = 7
	maxRetryDelay  = 10 * time.Second
)

var mu sync.Mutex

func initLogger() {
	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("[FATAL] Falha ao abrir arquivo de log: %v", err)
	}
	log.SetOutput(io.MultiWriter(os.Stdout, f))
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func handleSummary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	f, err := os.Open(summaryFile)
	if err != nil {
		http.Error(w, "Erro ao abrir histórico", http.StatusInternalServerError)
		log.Printf("[ERRO] Falha ao abrir o arquivo de resumo: %v", err)
		return
	}
	defer f.Close()

	w.Write([]byte("["))
	first := true
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if !first {
			w.Write([]byte(","))
		}
		first = false
		w.Write(scanner.Bytes())

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[ERRO] Falha ao escanear o arquivo de resumo: %v", err)
		http.Error(w, "Erro ao ler histórico", http.StatusInternalServerError)
		return
	}

	w.Write([]byte("]"))
	log.Printf("[INFO] Resumo de pagamentos retornado com sucesso")
}

func handlePayment(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Erro ao ler requisição", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var p Payment
	if err := json.Unmarshal(body, &p); err != nil || p.ID == "" {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}

	dest := filepath.Join(queueDir, p.ID+".json")
	if err := os.WriteFile(dest, body, 0644); err != nil {
		http.Error(w, "Erro ao enfileirar pagamento", http.StatusInternalServerError)
		log.Printf("[ERRO] Falha ao gravar arquivo: %v", err)
		return
	}

	log.Printf("[INFO] Pagamento recebido e enfileirado: %s", p.ID)
	w.WriteHeader(http.StatusAccepted)
}

func sendToEndpoint(url string, p Payment) bool {
	payload, _ := json.Marshal(p)
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[WARN] Falha ao enviar para %s: %v", url, err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Printf("[INFO] Pagamento %s enviado com sucesso para %s", p.ID, url)
		return true
	}
	log.Printf("[WARN] Pagamento %s falhou com status %d em %s", p.ID, resp.StatusCode, url)
	return false
}

func appendToSummary(p Payment) {
	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(summaryFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[ERRO] Falha ao abrir arquivo de resumo: %v", err)
		return
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	if err := enc.Encode(p); err != nil {
		log.Printf("[ERRO] Falha ao escrever no arquivo de resumo: %v", err)
	}
}

func processQueue(id int, dir string) {
	for {
		files, err := os.ReadDir(dir)
		if err != nil {
			log.Printf("[Worker %d] Erro ao ler diretório da fila: %v", id, err)
			continue
		}

		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
				continue
			}

			path := filepath.Join(dir, f.Name())
			lockPath := path + ".lock"
			lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				continue
			}
			lockFile.Close()

			data, err := os.ReadFile(path)
			if err != nil {
				log.Printf("[Worker %d] Falha ao ler arquivo: %v", id, err)
				os.Remove(lockPath)
				continue
			}

			var p Payment
			if err := json.Unmarshal(data, &p); err != nil {
				log.Printf("[Worker %d] Falha ao parsear JSON: %v", id, err)
				os.Remove(lockPath)
				continue
			}

			success := false
			delay := 100 * time.Millisecond
			for attempt := 1; attempt <= 5; attempt++ {
				if sendToEndpoint(mainURL, p) || sendToEndpoint(fallbackURL, p) {
					success = true
					break
				}
				time.Sleep(delay)
				delay *= 2
				if delay > maxRetryDelay {
					delay = maxRetryDelay
				}
			}

			if success {
				os.Remove(path)
				appendToSummary(p)
				log.Printf("[Worker %d] Processado com sucesso: %s", id, p.ID)
			} else {
				priorityPath := filepath.Join(priorityDir, filepath.Base(path))
				os.Rename(path, priorityPath)
				log.Printf("[Worker %d] Falha após tentativas. Movido para prioridade: %s", id, p.ID)
			}

			os.Remove(lockPath)
		}
		runtime.Gosched()
	}
}

func startWorkers(n int) {
	for i := 0; i < n; i++ {
		id := i + 1
		go processQueue(id, priorityDir)
		go processQueue(id, queueDir)
	}
}

func main() {
	initLogger()

	os.MkdirAll(queueDir, 0755)
	os.MkdirAll(priorityDir, 0755)
	os.MkdirAll(filepath.Dir(summaryFile), 0755)
	os.MkdirAll(filepath.Dir(logFilePath), 0755)

	http.HandleFunc("/payments", handlePayment)
	http.HandleFunc("/payments-summary", handleSummary)

	startWorkers(workerCount)

	log.Println("[INFO] Servidor escutando em :8081")
	if err := http.ListenAndServe(":8081", nil); err != nil {
		log.Fatalf("[FATAL] Falha ao iniciar servidor: %v", err)
	}
}
