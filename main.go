package main

import (
	"archive/zip"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

// getLocalIP ищет реальный локальный IPv4 адрес (LAN), отсекая VPN, Docker и отключенные интерфейсы.
func getLocalIP() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	// Черный список имен для виртуальных интерфейсов
	excludedPrefixes := []string{"docker", "br-", "veth", "virbr", "vboxnet"}

	for _, iface := range interfaces {
		// 1. Пропускаем отключенные интерфейсы, локалхост, VPN и PPPoE
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}

		// 2. Фильтруем по имени (Docker, VirtualBox и т.д.)
		// Дополнительная проверка на случай, если виртуалка не отмечена как P2P
		skip := false
		for _, prefix := range excludedPrefixes {
			if strings.HasPrefix(iface.Name, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Если интерфейс подошел, берем его адрес
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ipnet.IP.To4() != nil {
					return ipnet.IP.String()
				}
			}
		}
	}

	return ""
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
	// 1. Получаем путь к настройкам VS Code
	vscodePath, err := getVSCodePath()
	if err != nil {
		fmt.Printf("Ошибка поиска папки VS Code: %v\n", err)
		return
	}

	// 2. Получаем IP-адрес
	localIP := getLocalIP()
	if localIP == "" {
		fmt.Printf("Ошибка при старте сервера: не определен ip-адрес для подключения")
		return
	}

	// 3. Формируем красивую строку и выводим пользователю
	displayAddr := fmt.Sprintf("%s:%s", localIP, port)

	fmt.Println("========================================")
	fmt.Printf("✅ Сервер успешно запущен!\n")
	fmt.Printf("⚠️  На клиенте используйте команду:\n")
	fmt.Printf("> vscode-settings-sync client %s\n", displayAddr)
	fmt.Println("========================================")
	fmt.Println("Ожидание подключений...")

	// 4. Настраиваем HTTP-обработчик
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

	// 5. Создаем сервер и запускаем в отдельной горутине
	srv := &http.Server{Addr: ":" + port}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Ошибка сервера: %v\n", err)
		}
	}()

	// 6. Ожидаем сигнал завершения (Ctrl+C)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit // Программа "замрет" здесь, пока не нажмешь Ctrl+C

	fmt.Println("\n🛑 Получен сигнал остановки сервера. Ждем завершения текущих загрузок...")

	// 7. Даем серверу 5 секунд на закрытие всех текущих коннектов
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Printf("Ошибка при закрытии сервера: %v\n", err)
	}

	fmt.Println("✅ Сервер успешно остановлен.")

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
	// flag.String возвращает *string (указатель).
	portPtr := flag.String("port", DEFAULT_PORT, "Порт для работы сервера/клиента")
	flag.Parse() // Парсим флаги, которые пользователь передал при запуске

	// После flag.Parse оставшиеся аргументы лежат в os.Args
	// os.Args[0] - имя программы
	// os.Args[1] - первая команда (server/client), если есть.
	// os.Args[2] - вторая команда (IP), если есть.

	if len(os.Args) < 2 {
		fmt.Println("Использование:")
		fmt.Println("  Сервер: vscode-settings-sync [-port ПОРТ] server")
		fmt.Println("  Клиент: vscode-settings-sync [-port ПОРТ] client <IP-адрес-сервера>")
		fmt.Println("\nПримеры:")
		fmt.Println("  vscode-settings-sync server")
		fmt.Println("  vscode-settings-sync -port 9000 client 192.168.1.50")
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
			fmt.Println("Пример: vscode-settings-sync client 192.168.1.50")
			return
		}
		ip := os.Args[2]
		runClient(ip, *portPtr)
	default:
		fmt.Println("Неизвестная команда. Используйте 'server' или 'client'.")
	}
}
