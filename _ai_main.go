package main

import (
	"archive/zip"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Задаем значение по умолчанию
const (
	DEFAULT_PORT = "8080"
)

// getVSCodePath возвращает путь к директории User настроек VS Code
func getVSCodePath() (string, error) {
	var basePath string

	switch runtime.GOOS {
	case "windows":
		basePath = os.Getenv("APPDATA")
		if basePath == "" {
			return "", fmt.Errorf("не удалось получить переменную APPDATA")
		}
		return filepath.Join(basePath, "Code", "User"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Code", "User"), nil
	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "Code", "User"), nil
	default:
		return "", fmt.Errorf("неподдерживаемая ОС: %s", runtime.GOOS)
	}
}

// zipSource архивирует папку source в байтовый буфер
func zipSource(source string) (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	walker := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		// Создаем путь внутри архива относительно папки source
		f, err := w.Create(path[len(source):])
		if err != nil {
			return err
		}

		_, err = io.Copy(f, file)
		return err
	}

	err := filepath.Walk(source, walker)
	if err != nil {
		return nil, err
	}

	err = w.Close()
	if err != nil {
		return nil, err
	}

	return buf, nil
}

// unzipDest распаковывает архив из reader в папку dest
func unzipDest(reader io.Reader, dest string) error {
	os.MkdirAll(dest, 0755)

	buf := new(bytes.Buffer)
	buf.ReadFrom(reader)

	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return err
	}

	for _, f := range r.File {
		fpath := filepath.Join(dest, f.Name)

		// Защита от ZipSlip
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("недопустимый путь файла: %s", fpath)
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, f.Mode())
			continue
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()

		if err != nil {
			return err
		}
	}
	return nil
}

func backupDir(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	timestamp := time.Now().Format("20060102-150405")
	dest := path + "_backup_" + timestamp

	fmt.Printf("Создание бэкапа текущих настроек в: %s\n", dest)
	return os.Rename(path, dest)
}

// --- СЕРВЕРНАЯ ЧАСТЬ ---

func runServer(port string) {
	vscodePath, err := getVSCodePath()
	if err != nil {
		fmt.Printf("Ошибка поиска папки VS Code: %v\n", err)
		return
	}

	http.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Только GET запросы", http.StatusMethodNotAllowed)
			return
		}

		fmt.Println("Запрос на синхронизацию получен. Подготовка архива...")

		zipData, err := zipSource(vscodePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=vscode_settings.zip")
		w.Write(zipData.Bytes())

		fmt.Println("Архив отправлен.")
	})

	fmt.Printf("Сервер запущен на порту %s. Ожидание подключений...\n", port)
	fmt.Printf("Папка настроек: %s\n", vscodePath)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Printf("Ошибка сервера: %v\n", err)
	}
}

// --- КЛИЕНТСКАЯ ЧАСТЬ ---

func runClient(serverIP string, port string) {
	vscodePath, err := getVSCodePath()
	if err != nil {
		fmt.Printf("Ошибка поиска папки VS Code: %v\n", err)
		return
	}

	url := fmt.Sprintf("http://%s:%s/sync", serverIP, port)
	fmt.Printf("Подключение к серверу: %s\n", url)

	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("Ошибка подключения к серверу: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Сервер вернул ошибку: %s\n", resp.Status)
		return
	}

	if err := backupDir(vscodePath); err != nil {
		fmt.Printf("Внимание: не удалось создать бэкап: %v\n", err)
	}

	fmt.Println("Распаковка полученных настроек...")
	if err := unzipDest(resp.Body, vscodePath); err != nil {
		fmt.Printf("Ошибка распаковки: %v\n", err)
		return
	}

	fmt.Println("Синхронизация успешно завершена! Перезапустите VS Code.")
}

// --- MAIN ---

func main() {
	// Определяем флаг для порта
	portPtr := flag.String("port", DEFAULT_PORT, "Порт для работы сервера/клиента")
	flag.Parse() // Парсим флаги, которые пользователь передал при запуске

	// После flag.Parse оставшиеся аргументы лежат в os.Args
	// os.Args[0] - имя программы
	// os.Args[1] - первая команда (server/client)
	// os.Args[2] - вторая команда (IP)

	if len(os.Args) < 2 {
		fmt.Println("Использование:")
		fmt.Println("  Сервер: vsc-sync [-port ПОРТ] server")
		fmt.Println("  Клиент: vsc-sync [-port ПОРТ] client <IP-адрес-сервера>")
		fmt.Println("\nПо умолчанию используется порт 8080.")
		return
	}

	command := os.Args[1]

	switch command {
	case "server":
		runServer(*portPtr)
	case "client":
		if len(os.Args) < 3 {
			fmt.Println("Ошибка: укажите IP адрес сервера.")
			fmt.Println("Пример: vsc-sync client 192.168.1.50")
			return
		}
		ip := os.Args[2]
		runClient(ip, *portPtr)
	default:
		fmt.Println("Неизвестная команда. Используйте 'server' или 'client'.")
	}
}
