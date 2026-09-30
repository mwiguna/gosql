<h1 align="center">GoSQL</h1>

<h3 align="center">Your databases. One focused workspace.</h3>

<p align="center">
  <img src="web/assets/postgresql.svg" alt="PostgreSQL" width="46" height="46">
  &nbsp;&nbsp;
  <img src="web/assets/mysql.svg" alt="MySQL and MariaDB" width="46" height="46">
  &nbsp;&nbsp;
  <img src="web/assets/sqlite.svg" alt="SQLite" width="46" height="46">
</p>

<p align="center"><strong>PostgreSQL · MySQL · MariaDB · SQLite</strong></p>

<p align="center">
  <a href="../../releases"><strong>↓ Download for macOS &amp; Windows</strong></a>
  &nbsp;·&nbsp;
  <a href="#get-started">Get started</a>
  &nbsp;·&nbsp;
  <a href="#build-it-yourself">Build from source</a>
</p>

---

GoSQL brings your data into one clear workspace in your browser. Explore tables, make changes, and move between databases without juggling different tools.

Everything runs from a single executable. Start GoSQL, open the local address in your browser, and you're ready to work.

## What can you do with it?

- Browse databases, tables, and views.
- View and edit rows, table columns, indexes, and constraints.
- Run SQL queries and keep your connection profiles and workspace in one place.
- Open an existing SQLite file directly, or upload a copy to work on separately.

## Get started

Download the file for your computer from [GitHub Releases](../../releases):

| Your computer | Download |
| --- | --- |
| Mac with Apple Silicon | `gosql-macos-arm64` |
| Windows 64-bit | `gosql-windows-amd64.exe` |

Put the file in a folder where you'd like to keep GoSQL, then open Terminal or PowerShell in that folder and run it:

**macOS**

```sh
chmod +x ./gosql-macos-arm64
./gosql-macos-arm64
```

**Windows (PowerShell)**

```powershell
.\gosql-windows-amd64.exe
```

Open **http://127.0.0.1:8080** in your browser. The first time you visit, GoSQL will ask you to create an admin account. Keep the Terminal or PowerShell window open while you're using the app; press `Ctrl+C` when you're done.

GoSQL creates a `data` folder next to the executable for your account, connection profiles, and workspace. Keep that folder when you update or move the app. If macOS blocks the downloaded file, you can allow it in **System Settings → Privacy & Security**.

> **Working with SQLite?** If you enter a path, use the absolute path to a file **on the machine running GoSQL (the server)**. When you access GoSQL from another computer, a path to a file on your own computer will not work. GoSQL edits the server file directly; choose **Upload Copy** if your file is only on your computer or you'd rather work on a separate copy.

## Build it yourself

You'll need **Go 1.27.1 or newer**, plus **Node.js and npm**. From the project root, run:

```sh
npm ci
npm run build
cd backend
go build -o gosql .
```

On Windows, use `go build -o gosql.exe .` for the last command. The web interface is bundled into the executable, so run `npm run build` again whenever you change the frontend before rebuilding GoSQL.

## Under the hood

GoSQL uses **Go** for the backend and **HTML, JavaScript, and CSS** for the interface. It uses **SQLite** to store app accounts, connection profiles, and workspace data locally.

By default, the browser interface is available only on the computer running GoSQL. To make it available on your local network, start it with `-addr 0.0.0.0:8080`.
