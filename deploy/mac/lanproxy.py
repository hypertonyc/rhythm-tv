import asyncio
import sys


async def pipe(reader, writer):
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            writer.write(data)
            await writer.drain()
        if writer.can_write_eof():
            writer.write_eof()
    except (ConnectionError, OSError):
        writer.close()


async def main(listen_host, listen_port, up_host, up_port):
    async def handle(client_reader, client_writer):
        try:
            up_reader, up_writer = await asyncio.open_connection(up_host, up_port)
        except OSError:
            client_writer.close()
            return
        await asyncio.gather(pipe(client_reader, up_writer), pipe(up_reader, client_writer))
        up_writer.close()
        client_writer.close()

    server = await asyncio.start_server(handle, listen_host, listen_port, reuse_address=True)
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    asyncio.run(main(sys.argv[1], int(sys.argv[2]), sys.argv[3], int(sys.argv[4])))
